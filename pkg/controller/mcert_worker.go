/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"

	api "managed-certs-gke/pkg/apis/gke.googleapis.com/v1alpha1"
	"managed-certs-gke/pkg/utils"
)

const (
	sslActive                              = "ACTIVE"
	sslFailedNotVisible                    = "FAILED_NOT_VISIBLE"
	sslFailedCaaChecking                   = "FAILED_CAA_CHECKING"
	sslFailedCaaForbidden                  = "FAILED_CAA_FORBIDDEN"
	sslFailedRateLimited                   = "FAILED_RATE_LIMITED"
	sslManagedCertificateStatusUnspecified = "MANAGED_CERTIFICATE_STATUS_UNSPECIFIED"
	sslProvisioning                        = "PROVISIONING"
	sslProvisioningFailed                  = "PROVISIONING_FAILED"
	sslProvisioningFailedPermanently       = "PROVISIONING_FAILED_PERMANENTLY"
	sslRenewalFailed                       = "RENEWAL_FAILED"
)

func translateDomainStatus(status string) (string, error) {
	switch status {
	case sslProvisioning:
		return "Provisioning", nil
	case sslFailedNotVisible:
		return "FailedNotVisible", nil
	case sslFailedCaaChecking:
		return "FailedCaaChecking", nil
	case sslFailedCaaForbidden:
		return "FailedCaaForbidden", nil
	case sslFailedRateLimited:
		return "FailedRateLimited", nil
	case sslActive:
		return "Active", nil
	default:
		return "", fmt.Errorf("Unexpected status %s", status)
	}
}

func (c *McertController) updateStatus(mcert *api.ManagedCertificate) error {
	sslCertificateName, exists := c.state.Get(mcert.Name)
	if !exists {
		return fmt.Errorf("Failed to find in state Managed Certificate %s", mcert.Name)
	}

	sslCert, err := c.sslClient.Get(sslCertificateName)
	if err != nil {
		return err
	}

	switch sslCert.Managed.Status {
	case sslActive:
		mcert.Status.CertificateStatus = "Active"
	case sslManagedCertificateStatusUnspecified, "":
		mcert.Status.CertificateStatus = ""
	case sslProvisioning:
		mcert.Status.CertificateStatus = "Provisioning"
	case sslProvisioningFailed:
		mcert.Status.CertificateStatus = "ProvisioningFailed"
	case sslProvisioningFailedPermanently:
		mcert.Status.CertificateStatus = "ProvisioningFailedPermanently"
	case sslRenewalFailed:
		mcert.Status.CertificateStatus = "RenewalFailed"
	default:
		return fmt.Errorf("Unexpected status %s of SslCertificate %v", sslCert.Managed.Status, sslCert)
	}

	var domainStatus []api.DomainStatus
	for domain, status := range sslCert.Managed.DomainStatus {
		translatedStatus, err := translateDomainStatus(status)
		if err != nil {
			return err
		}

		domainStatus = append(domainStatus, api.DomainStatus{
			Domain: domain,
			Status: translatedStatus,
		})
	}
	mcert.Status.DomainStatus = domainStatus
	mcert.Status.CertificateName = sslCert.Name

	_, err = c.client.GkeV1alpha1().ManagedCertificates(mcert.Namespace).Update(mcert)
	return err
}

func (c *McertController) createSslCertificateIfNeeded(sslCertificateName string, mcert *api.ManagedCertificate) error {
	if _, err := c.sslClient.Get(sslCertificateName); err != nil {
		//SslCertificate does not yet exist, create it
		glog.Infof("McertController creates a new SslCertificate %s associated with Managed Certificate %s, based on state", sslCertificateName, mcert.Name)
		if err := c.sslClient.Create(sslCertificateName, mcert.Spec.Domains); err != nil {
			return err
		}
	}

	return nil
}

func (c *McertController) createSslCertificateNameIfNeeded(name string) (string, error) {
	sslCertificateName, exists := c.state.Get(name)

	if exists && sslCertificateName != "" {
		return sslCertificateName, nil
	}

	//State does not have anything for this managed certificate or no SslCertificate is associated with it
	sslCertificateName, err := c.randomName()
	if err != nil {
		return "", err
	}

	glog.Infof("McertController adds to state new SslCertificate name %s associated with Managed Certificate %s", sslCertificateName, name)
	c.state.Put(name, sslCertificateName)
	return sslCertificateName, nil
}

func (c *McertController) handleMcert(key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	glog.Infof("McertController handling Managed Certificate %s:%s", ns, name)

	mcert, err := c.lister.ManagedCertificates(ns).Get(name)
	if err != nil {
		return err
	}

	sslCertificateName, err := c.createSslCertificateNameIfNeeded(name)
	if err != nil {
		return err
	}

	if err = c.createSslCertificateIfNeeded(sslCertificateName, mcert); err != nil {
		return err
	}

	return c.updateStatus(mcert)
}

func (c *McertController) processNext() bool {
	obj, shutdown := c.queue.Get()

	if shutdown {
		return false
	}

	defer c.queue.Done(obj)

	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		c.queue.Forget(obj)
		runtime.HandleError(fmt.Errorf("Expected string in mcertQueue but got %T", obj))
	}

	if err := c.handleMcert(key); err != nil {
		c.queue.AddRateLimited(obj)
		runtime.HandleError(err)
	}

	c.queue.Forget(obj)

	return true
}

func (c *McertController) runWorker() {
	for c.processNext() {
	}
}

func (c *McertController) randomName() (string, error) {
	name, err := utils.RandomName()
	if err != nil {
		return "", err
	}

	_, err = c.sslClient.Get(name)
	if err == nil {
		//Name taken, choose a new one
		name, err = utils.RandomName()
		if err != nil {
			return "", err
		}
	}

	return name, nil
}
