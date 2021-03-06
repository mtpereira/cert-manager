/*
Copyright 2018 The Jetstack cert-manager contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package acme

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/golang/glog"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/jetstack/cert-manager/pkg/acme"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/issuer"
	"github.com/jetstack/cert-manager/pkg/util/errors"
	"github.com/jetstack/cert-manager/pkg/util/kube"
	"github.com/jetstack/cert-manager/pkg/util/pki"
)

const (
	createOrderWaitDuration = time.Minute * 5
)

var (
	certificateGvk = v1alpha1.SchemeGroupVersion.WithKind("Certificate")
)

func (a *Acme) Issue(ctx context.Context, crt *v1alpha1.Certificate) (issuer.IssueResponse, error) {
	// initially, we do not set the csr on the order resource we build.
	// this is to save having the overhead of generating a new CSR in the case
	// where the Order resource is up to date already
	expectedOrder, err := buildOrder(crt, nil)
	if err != nil {
		return issuer.IssueResponse{}, err
	}

	// attempt to retrieve the existing order resource for this Certificate
	var existingOrder *v1alpha1.Order

	glog.V(4).Infof("Attempting to retrieve existing orders for %q/%q", crt.Namespace, crt.Name)
	labelMap := certLabels(crt.Name)
	selector := labels.NewSelector()
	for k, v := range labelMap {
		req, err := labels.NewRequirement(k, selection.Equals, []string{v})
		if err != nil {
			return issuer.IssueResponse{}, err
		}
		selector.Add(*req)
	}
	existingOrders, err := a.orderLister.Orders(crt.Namespace).List(selector)
	if err != nil {
		return issuer.IssueResponse{}, err
	}
	for _, o := range existingOrders {
		// Don't touch any objects that don't have this certificate set as the
		// owner reference.
		if !metav1.IsControlledBy(o, crt) {
			continue
		}

		if o.Name == expectedOrder.Name {
			existingOrder = o
			glog.V(4).Infof("Found existing order %q for certificate %s/%s", existingOrder.Name, crt.Namespace, crt.Name)
			continue
		}

		// delete any old order resources
		glog.Infof("Deleting Order resource %s/%s", o.Namespace, o.Name)
		err := a.CMClient.CertmanagerV1alpha1().Orders(o.Namespace).Delete(o.Name, nil)
		if err != nil {
			return issuer.IssueResponse{}, err
		}
	}

	// get existing certificate private key
	glog.V(4).Infof("Attempting to fetch existing certificate private key")
	key, err := kube.SecretTLSKey(a.secretsLister, crt.Namespace, crt.Spec.SecretName)
	if err != nil {
		// if the private key is not found, or is formatted incorrectly, we
		// will generate a new one and create/overwrite the secret.
		if !apierrors.IsNotFound(err) && !errors.IsInvalidData(err) {
			return issuer.IssueResponse{}, err
		}

		// TODO: perhaps we shouldn't overwrite the secret if it is invalid,
		// and instead bail out here?

		glog.V(4).Infof("Generating new private key for %s/%s", crt.Namespace, crt.Name)
		// generate a new private key.
		key, err := pki.GenerateRSAPrivateKey(2048)
		if err != nil {
			return issuer.IssueResponse{}, err
		}

		keyBytes, err := pki.EncodePrivateKey(key)
		if err != nil {
			return issuer.IssueResponse{}, err
		}

		glog.V(4).Infof("Storing new certificate private key for %s/%s", crt.Namespace, crt.Name)
		// We return the private key here early, and trigger an immediate requeue.
		// This is because we have just generated a new one, and to keep our
		// later logic simple we store it immediately.
		// TODO: remove the 'requeue: true' as it could cause a race condition
		// where our lister has not observed the new private key generated above
		return issuer.IssueResponse{PrivateKey: keyBytes}, nil
	}

	// if there is an existing order, we check to make sure it is up to date
	// with the current certificate & issuer configuration.
	// if it is not, we will abandon the old order and create a new one.
	// The 'retry' cases here will bypass the controller's rate-limiting, as
	// well as the back-off applied to failing ACME Orders.
	// They should therefore *only* match on changes to the actual Certificate
	// resource, or underlying Order (i.e. user interaction).
	if existingOrder != nil {
		glog.V(4).Infof("Validating existing order CSR for Certificate %s/%s", crt.Namespace, crt.Name)

		// if the existing order has expired, we should create a new one
		// TODO: implement this order state in the acmeorders controller
		if existingOrder.Status.State == v1alpha1.Expired {
			return a.retryOrder(existingOrder)
		}

		// check if the existing order has failed.
		// If it has, we check to see when it last failed and 'back-off' if it
		// failed in the recent past.
		if acme.IsFailureState(existingOrder.Status.State) {
			if crt.Status.LastFailureTime == nil {
				nowTime := metav1.NewTime(a.clock.Now())
				crt.Status.LastFailureTime = &nowTime
			}

			if time.Now().Sub(crt.Status.LastFailureTime.Time) < createOrderWaitDuration {
				return issuer.IssueResponse{}, fmt.Errorf("applying acme order back-off for certificate %s/%s because it has failed within the last %s", crt.Namespace, crt.Name, createOrderWaitDuration)
			}

			// otherwise, we clear the lastFailureTime and create a new order
			// as the back-off time has passed.
			crt.Status.LastFailureTime = nil

			return a.retryOrder(existingOrder)
		}

		// check the CSR is created by the private key that we hold
		csrBytes := existingOrder.Spec.CSR
		if len(csrBytes) == 0 {
			return a.retryOrder(existingOrder)
		}
		existingCSR, err := x509.ParseCertificateRequest(csrBytes)
		if err != nil {
			return a.retryOrder(existingOrder)
		}

		matches, err := pki.PublicKeyMatchesCSR(key.Public(), existingCSR)
		if err != nil {
			return issuer.IssueResponse{}, err
		}

		if !matches {
			glog.V(4).Infof("CSR on existing order resource does not match certificate %s/%s private key. Creating new order.", crt.Namespace, crt.Name)
			return a.retryOrder(existingOrder)
		}
	}

	if existingOrder == nil {
		glog.V(4).Infof("Creating new Order resource for Certificate %s/%s", crt.Namespace, crt.Name)

		csr, err := pki.GenerateCSR(a.issuer, crt)
		if err != nil {
			// TODO: what errors can be produced here? some error types might
			// be permenant, and we should handle that properly.
			return issuer.IssueResponse{}, err
		}

		csrBytes, err := pki.EncodeCSR(csr, key)
		if err != nil {
			return issuer.IssueResponse{}, err
		}

		// set the CSR field on the order to be created
		expectedOrder.Spec.CSR = csrBytes

		existingOrder, err = a.CMClient.CertmanagerV1alpha1().Orders(crt.Namespace).Create(expectedOrder)
		if err != nil {
			return issuer.IssueResponse{}, err
		}

		glog.V(4).Infof("Created new Order resource named %q for Certificate %s/%s", expectedOrder.Name, crt.Namespace, crt.Name)

		return issuer.IssueResponse{}, nil
	}

	if existingOrder.Status.State != v1alpha1.Valid {
		glog.Infof("Order %s/%s is not in 'valid' state. Waiting for Order to transition before attempting to issue Certificate.", existingOrder.Namespace, existingOrder.Name)

		// We don't immediately requeue, as the change to the Order resource on
		// transition should trigger the certificate to be re-synced.
		return issuer.IssueResponse{}, nil
	}

	// If the order is valid, we can attempt to retrieve the Certificate.
	// First obtain an ACME client to make this easier.
	cl, err := a.helper.ClientForIssuer(a.issuer)
	if err != nil {
		return issuer.IssueResponse{}, err
	}

	certSlice, err := cl.GetCertificate(ctx, existingOrder.Status.CertificateURL)
	if err != nil {
		// TODO: parse returned ACME error and potentially re-create order.
		return issuer.IssueResponse{}, err
	}

	if len(certSlice) == 0 {
		// TODO: parse returned ACME error and potentially re-create order.
		return issuer.IssueResponse{}, fmt.Errorf("invalid certificate returned from acme server")
	}

	x509Cert, err := x509.ParseCertificate(certSlice[0])
	if err != nil {
		// TODO: parse returned ACME error and potentially re-create order.
		return issuer.IssueResponse{}, fmt.Errorf("failed to parse returned x509 certificate: %v", err.Error())
	}

	if a.Context.IssuerOptions.CertificateNeedsRenew(x509Cert) {
		// existing order's certificate is near expiry
		return a.retryOrder(existingOrder)
	}

	// encode the retrieved certificates (including the chain)
	certBuffer := bytes.NewBuffer([]byte{})
	for _, cert := range certSlice {
		err := pem.Encode(certBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: cert})
		if err != nil {
			return issuer.IssueResponse{}, err
		}
	}

	// encode the private key and return
	keyPem, err := pki.EncodePrivateKey(key)
	if err != nil {
		// TODO: this is probably an internal error - we should fail safer here
		return issuer.IssueResponse{}, err
	}

	return issuer.IssueResponse{
		Certificate: certBuffer.Bytes(),
		PrivateKey:  keyPem,
	}, nil
}

// retryOrder will delete the existing order with the foreground
// deletion policy.
// If delete successfully (i.e. cleaned up), the order name will be
// reset to empty and a resync of the resource will begin.
func (a *Acme) retryOrder(existingOrder *v1alpha1.Order) (issuer.IssueResponse, error) {
	foregroundDeletion := metav1.DeletePropagationForeground
	err := a.CMClient.CertmanagerV1alpha1().Orders(existingOrder.Namespace).Delete(existingOrder.Name, &metav1.DeleteOptions{
		PropagationPolicy: &foregroundDeletion,
	})
	if err != nil {
		return issuer.IssueResponse{}, err
	}

	// Updating the certificate status will trigger a requeue once the change
	// has been observed by the informer.
	// If we set Requeue: true here, we may cause a race where the lister has
	// not observed the updated orderRef.
	return issuer.IssueResponse{}, nil
}

func buildOrder(crt *v1alpha1.Certificate, csr []byte) (*v1alpha1.Order, error) {
	spec := v1alpha1.OrderSpec{
		CSR:        csr,
		IssuerRef:  crt.Spec.IssuerRef,
		CommonName: crt.Spec.CommonName,
		DNSNames:   crt.Spec.DNSNames,
		Config:     crt.Spec.ACME.Config,
	}
	hash, err := hashOrder(spec)
	if err != nil {
		return nil, err
	}

	return &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-%d", crt.Name, hash),
			Namespace:       crt.Namespace,
			Labels:          certLabels(crt.Name),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(crt, certificateGvk)},
		},
		Spec: spec,
	}, nil
}

func certLabels(crtName string) map[string]string {
	return map[string]string{
		"acme.cert-manager.io/certificate-name": crtName,
	}
}

func hashOrder(orderSpec v1alpha1.OrderSpec) (uint32, error) {
	// create a shallow copy of the OrderSpec so we can overwrite the CSR field
	orderSpec.CSR = nil

	orderSpecBytes, err := json.Marshal(orderSpec)
	if err != nil {
		return 0, err
	}

	hashF := fnv.New32()
	_, err = hashF.Write(orderSpecBytes)
	if err != nil {
		return 0, err
	}

	return hashF.Sum32(), nil
}
