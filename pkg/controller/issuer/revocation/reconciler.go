/*
 * SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package revocation

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/gardener/controller-manager-library/pkg/controllermanager/cluster"
	"github.com/gardener/controller-manager-library/pkg/controllermanager/controller"
	"github.com/gardener/controller-manager-library/pkg/controllermanager/controller/reconcile"
	"github.com/gardener/controller-manager-library/pkg/logger"
	"github.com/gardener/controller-manager-library/pkg/resources"

	api "github.com/gardener/cert-management/pkg/apis/cert/v1alpha1"
	"github.com/gardener/cert-management/pkg/cert/legobridge"
	"github.com/gardener/cert-management/pkg/cert/source"
	"github.com/gardener/cert-management/pkg/cert/utils"
	ctrl "github.com/gardener/cert-management/pkg/controller"
	"github.com/gardener/cert-management/pkg/controller/issuer/certificate"
	"github.com/gardener/cert-management/pkg/controller/issuer/core"
)

// RevokeReconciler creates a certificate revocation reconciler.
func RevokeReconciler(c controller.Interface, support *core.Support) (reconcile.Interface, error) {
	targetCluster := c.GetCluster(ctrl.TargetCluster)
	certResources, err := targetCluster.Resources().GetByExample(&api.Certificate{})
	if err != nil {
		return nil, err
	}
	certSecretResources, err := targetCluster.Resources().GetByExample(&corev1.Secret{})
	if err != nil {
		return nil, err
	}
	certRevocationResources, err := targetCluster.Resources().GetByExample(&api.CertificateRevocation{})
	if err != nil {
		return nil, err
	}

	copt, _ := c.GetStringOption(source.OptClass)
	classes := controller.NewClasses(c, copt, source.AnnotClass, source.DefaultClass)

	dnsCluster := c.GetCluster(ctrl.DNSCluster)
	reconciler := &revokeReconciler{
		support:                 support,
		obtainer:                legobridge.NewObtainer(),
		classes:                 classes,
		dnsCluster:              dnsCluster,
		certResources:           certResources,
		certRevocationResources: certRevocationResources,
		certSecretResources:     certSecretResources,
	}

	return reconciler, nil
}

type recoverableError struct {
	Msg      string
	Interval time.Duration
}

func (err *recoverableError) Error() string {
	return err.Msg
}

type revokeReconciler struct {
	reconcile.DefaultReconciler
	support                 *core.Support
	obtainer                legobridge.Obtainer
	dnsCluster              cluster.Interface
	certResources           resources.Interface
	certSecretResources     resources.Interface
	certRevocationResources resources.Interface
	classes                 *controller.Classes
}

func (r *revokeReconciler) Start() {
}

func (r *revokeReconciler) Reconcile(logctx logger.LogContext, obj resources.Object) reconcile.Status {
	logctx.Infof("reconciling certificate revocation")
	revocation, ok := obj.Data().(*api.CertificateRevocation)
	if !ok {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("casting to CertificateRevocation failed"))
	}

	if !r.classes.IsResponsibleFor(logctx, obj) {
		logctx.Infof("not responsible")
		return reconcile.Succeeded(logctx)
	}

	if revocation.Status.RevocationApplied != nil {
		return reconcile.Succeeded(logctx)
	}

	if revocation.Spec.QualifyingDate == nil {
		revocation.Spec.QualifyingDate = &metav1.Time{Time: time.Now()}
		_, err := r.certRevocationResources.Update(revocation)
		if err != nil && !apierrrors.IsConflict(err) {
			return r.failed(logctx, obj, api.StateError, errors.Wrap(err, "updating certificate revocation resource failed"))
		}
		return reconcile.Recheck(logctx, nil, 500*time.Millisecond)
	}

	name := resources.NewObjectName(revocation.Spec.CertificateRef.Namespace, revocation.Spec.CertificateRef.Name)
	certObj, err := r.certResources.GetInto(name, &api.Certificate{})
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, err)
	}
	cert, ok := certObj.Data().(*api.Certificate)
	if !ok {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("casting to Certificate failed"))
	}
	if cert.Status.State == api.StateRevoked && !isInvolved(revocation, cert) {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("certficate already revoked"))
	}
	hashKey, ok := cert.Labels[certificate.LabelCertificateHashKey]
	if !ok {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("certificate has no %s label", certificate.LabelCertificateHashKey))
	}

	issuer, err := r.support.LoadIssuer(cert)
	if err != nil {
		return r.failed(logctx, obj, api.StateError, err)
	}
	if issuer.Spec.ACME == nil {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("certificate revocation only supported for issuers of type ACME"))
	}

	if revocation.Status.Secrets == nil {
		// find all valid certificate secrets to be revoked and store them in the status
		return r.collectSecretsRefsAndRepeat(logctx, obj, cert.Spec.SecretRef, issuer)
	}

	shouldRenewBeforeRevoke := revocation.Spec.Renew != nil && *revocation.Spec.Renew
	if revocation.Status.Objects == nil {
		// find all certificate objects using the certificate to be revoked and store them in the status
		// and trigger renewal for all certificates with same hash key if requested
		return r.collectCertificateRefsAndRepeat(logctx, obj, hashKey, shouldRenewBeforeRevoke)
	}

	if shouldRenewBeforeRevoke && len(revocation.Status.Objects.Processing) > 0 {
		// check all certificate objects are renewed, update status
		return r.checkRenewalReadyAndRepeat(logctx, obj)
	}

	if len(revocation.Status.Secrets.Processing) > 0 {
		return r.revokeOldCertificateSecrets(logctx, obj, issuer, hashKey, shouldRenewBeforeRevoke)
	}

	if !shouldRenewBeforeRevoke && len(revocation.Status.Objects.Processing) > 0 {
		// check all certificate objects are revoked
		return r.checkRevokedAndRepeat(logctx, obj)
	}

	return r.finishRevocation(logctx, obj, shouldRenewBeforeRevoke)
}

func (r *revokeReconciler) Deleted(logctx logger.LogContext, key resources.ClusterObjectKey) reconcile.Status {
	logctx.Infof("deleted")

	return reconcile.Succeeded(logctx)
}

func (r *revokeReconciler) collectSecretsRefsAndRepeat(logctx logger.LogContext, obj resources.Object,
	certSecretRef *corev1.SecretReference, issuer *api.Issuer) reconcile.Status {
	revocation := obj.Data().(*api.CertificateRevocation)
	if certSecretRef == nil {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("missing secret refernce of certificate"))
	}

	secret, err := r.loadSecret(certSecretRef)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "certificate secret"))
	}

	hashKey, ok := secret.Labels[certificate.LabelCertificateHashKey]
	if !ok {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("secret has no %s label", certificate.LabelCertificateHashKey))
	}

	// secret is already backed up on certificate creation, only needed for backwards compatibility
	issuerInfo := utils.NewIssuerInfoFromIssuer(issuer)
	_, _, err = certificate.BackupSecret(r.certSecretResources, secret, hashKey, issuerInfo)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "secret backup failed"))
	}

	secretRefs, err := certificate.FindAllOldBackupSecrets(r.certSecretResources, hashKey, revocation.Spec.QualifyingDate.Time)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "find all old secrets failed"))
	}

	return r.updateStatusAndDelay(logctx, obj, 1*time.Second, func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error) {
		msg := "looking up all certificates to process"
		mod, status := r.prepareUpdateStatus(obj, api.StatePending, &msg)
		status.Secrets = &api.SecretStatuses{Processing: secretRefs}
		return mod, nil
	})
}

func (r *revokeReconciler) collectCertificateRefsAndRepeat(logctx logger.LogContext, obj resources.Object, hashKey string,
	shouldRenewBeforeRevoke bool) reconcile.Status {
	revocation := obj.Data().(*api.CertificateRevocation)
	selector, err := createLabelCertificateHashKeySelector(hashKey)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "collectCertificateRefsAndRepeat"))
	}
	list, err := r.certResources.ListCached(selector)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "list certificates with same hash"))
	}

	qualifyingDate := *revocation.Spec.QualifyingDate
	var toBeProcessed []api.CertificateRef
	for _, certObj := range list {
		if !r.classes.IsResponsibleFor(logctx, certObj) {
			continue
		}
		cert := certObj.Data().(*api.Certificate)
		if cert.Status.State == api.StateRevoked {
			continue
		}
		secret, err := r.loadSecret(cert.Spec.SecretRef)
		if err != nil {
			if apierrrors.IsNotFound(errors.Cause(err)) {
				continue
			}
			return r.failedStop(logctx, obj, api.StateError, errors.Wrapf(err, "certificate %s/%s", cert.Namespace, cert.Name))
		}
		x509cert, err := legobridge.DecodeCertificateFromSecretData(secret.Data)
		if err != nil {
			return r.failedStop(logctx, obj, api.StateError, errors.Wrapf(err, "certificate %s/%s", cert.Namespace, cert.Name))
		}
		if !certificate.WasRequestedBefore(x509cert, qualifyingDate.Time) {
			continue
		}

		if shouldRenewBeforeRevoke {
			err = r.updateCertEnsureRenewedAfter(certObj, &qualifyingDate)
			if err != nil {
				if apierrrors.IsGone(err) {
					continue
				}
				return r.failedStop(logctx, obj, api.StateError, errors.Wrapf(err, "certificate %s/%s cannot be updated", cert.Namespace, cert.Name))
			}
		}

		toBeProcessed = append(toBeProcessed, api.CertificateRef{
			Name:      certObj.GetName(),
			Namespace: certObj.GetNamespace(),
		})
	}

	if len(toBeProcessed) == 0 {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("no certificates found to process"))
	}

	return r.updateStatusAndDelay(logctx, obj, 1*time.Second, func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error) {
		format := "found %d certificates to process"
		if shouldRenewBeforeRevoke {
			format = "renewing %d certificates"
		}
		msg := fmt.Sprintf(format, len(toBeProcessed))
		mod, status := r.prepareUpdateStatus(obj, api.StatePending, &msg)
		status.Objects = &api.ObjectStatuses{Processing: toBeProcessed}
		return mod, nil
	})
}

func (r *revokeReconciler) updateCertEnsureRenewedAfter(certObj resources.Object, renewedAfterTime *metav1.Time) error {
	var err error
	wait := 500 * time.Millisecond
	for i := 0; i < 5; i++ {
		cert := certObj.Data().(*api.Certificate)
		cert.Spec.EnsureRenewedAfter = renewedAfterTime
		err = certObj.Update()
		if err == nil {
			return nil
		}
		if !apierrrors.IsConflict(err) {
			return err
		}
		time.Sleep(wait)
		wait += 500 * time.Millisecond
		certObj, err = r.certResources.GetInto(certObj.ObjectName(), cert)
		if err != nil {
			return err
		}
	}
	return err
}

func (r *revokeReconciler) checkRenewalReadyAndRepeat(logctx logger.LogContext, obj resources.Object) reconcile.Status {
	revocation := obj.Data().(*api.CertificateRevocation)
	qualifyingDate := *revocation.Spec.QualifyingDate

	renewed := len(revocation.Status.Objects.Renewed)
	var stillToBeRenewed []api.CertificateRef
	for _, ref := range revocation.Status.Objects.Processing {
		name := resources.NewObjectName(ref.Namespace, ref.Name)
		cert := &api.Certificate{}
		_, err := r.certResources.GetInto(name, cert)
		if err == nil && cert.Spec.EnsureRenewedAfter != nil && !cert.Spec.EnsureRenewedAfter.Before(&qualifyingDate) && cert.Status.State == api.StateReady {
			revocation.Status.Objects.Renewed = append(revocation.Status.Objects.Renewed, ref)
		} else {
			if err != nil && !apierrrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				logctx.Warnf("retrieving certificate %s failed", name)
			}
			stillToBeRenewed = append(stillToBeRenewed, ref)
		}
	}

	return r.updateStatusAndDelay(logctx, obj, 15*time.Second, func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error) {
		msg := fmt.Sprintf("renewing certificate for %d certificate objects", len(stillToBeRenewed))
		if len(stillToBeRenewed) == 0 {
			msg = "revoking old certificate"
		}
		mod, status := r.prepareUpdateStatus(obj, api.StatePending, &msg)
		if len(status.Objects.Processing) != len(stillToBeRenewed) {
			status.Objects.Processing = stillToBeRenewed
			mod.Modified = true
		}
		if len(status.Objects.Renewed) > renewed {
			mod.Modified = true
		}
		return mod, nil
	})
}

func (r *revokeReconciler) checkRevokedAndRepeat(logctx logger.LogContext, obj resources.Object) reconcile.Status {
	revocation := obj.Data().(*api.CertificateRevocation)

	var stillPending []api.CertificateRef
	var revoked []api.CertificateRef
	var failed []api.CertificateRef
	for _, ref := range revocation.Status.Objects.Processing {
		name := resources.NewObjectName(ref.Namespace, ref.Name)
		cert := &api.Certificate{}
		_, err := r.certResources.GetInto(name, cert)
		if err != nil {
			failed = append(failed, ref)
			continue
		}
		switch cert.Status.State {
		case api.StateRevoked:
			revoked = append(revoked, ref)
		case api.StateError:
			failed = append(failed, ref)
		default:
			stillPending = append(stillPending, ref)
		}
	}
	delay := 5 * time.Second
	if len(stillPending) == 0 {
		delay = 500 * time.Millisecond
	}
	return r.updateStatusAndDelay(logctx, obj, delay, func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error) {
		mod, status := r.prepareUpdateStatus(obj, api.StatePending, revocation.Status.Message)
		if len(stillPending) != len(revocation.Status.Objects.Processing) {
			status.Objects.Processing = stillPending
			status.Objects.Revoked = append(status.Objects.Revoked, revoked...)
			status.Objects.Failed = append(status.Objects.Failed, failed...)
			mod.Modified = true
		}
		return mod, nil
	})
}

func (r *revokeReconciler) revokeOldCertificateSecrets(logctx logger.LogContext, obj resources.Object, issuer *api.Issuer,
	hashKey string, shouldRenewBeforeRevoke bool) reconcile.Status {
	revocation := obj.Data().(*api.CertificateRevocation)

	if len(revocation.Status.Secrets.Processing) == 0 {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("missing certificate secret references"))
	}

	user, caDirURL, err := r.support.RestoreRegUser(issuer)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, fmt.Errorf("cannot restore registration user from issuer"))
	}

	var revokedSerialNumbers []*big.Int
	var revokedSecrets []api.CertificateSecretRef
	var failedSecrets []api.CertificateSecretRef
	var errs []error
	for _, ref := range revocation.Status.Secrets.Processing {
		secret, err := r.loadSecret(&ref.SecretReference)
		if err != nil {
			if apierrrors.IsGone(err) {
				continue
			}
			errs = append(errs, errors.Wrapf(err, "cannot load backup certificate secret: %s", secret.Name))
			failedSecrets = append(failedSecrets, ref)
			continue
		}
		cert, err := legobridge.DecodeCertificateFromSecretData(secret.Data)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "cannot decode backup certificate secret %s", secret.Name))
			failedSecrets = append(failedSecrets, ref)
			continue
		}
		tlscrt := secret.Data[corev1.TLSCertKey]

		if _, ok := resources.GetAnnotation(secret, certificate.AnnotationRevoked); !ok {
			err = legobridge.RevokeCertificate(user, caDirURL, tlscrt)
			if err != nil {
				errs = append(errs, errors.Wrapf(err, "certificate revocation failed for backup certificate secret %s", secret.Name))
				failedSecrets = append(failedSecrets, ref)
				continue
			}
		}
		if cert.SerialNumber != nil {
			revokedSerialNumbers = append(revokedSerialNumbers, cert.SerialNumber)
		}
		revokedSecrets = append(revokedSecrets, ref)
		err = r.setCertificateSecretRevoked(secret)
		if err != nil {
			err = errors.Wrapf(err, "updating backup certificate secret %s failed", secret.Name)
			logctx.Warn(err.Error())
			errs = append(errs, err)
		}
	}

	err = r.setCertificateSecretsRevokedBySerialNumbers(hashKey, revokedSerialNumbers)
	if err != nil {
		err = errors.Wrap(err, "marking certificate secret as revoked failed")
		logctx.Warn(err.Error())
		errs = append(errs, err)
	}

	msg := "certificate(s) successfully revoked"
	if len(errs) > 0 {
		var errstrings []string
		for _, err := range errs {
			errstrings = append(errstrings, err.Error())
		}
		msg = fmt.Sprintf("errors during revocation: %s", strings.Join(errstrings, ", "))
	}

	if !shouldRenewBeforeRevoke {
		// trigger all certificate objects
		for _, ref := range revocation.Status.Objects.Processing {
			key := resources.NewClusterKey(r.certResources.GetCluster().GetId(), api.Kind(api.CertificateKind), ref.Namespace, ref.Name)
			r.support.EnqueueKey(key)
		}
	}

	return r.updateStatusAndDelay(logctx, obj, 1*time.Second, func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error) {
		mod, status := r.prepareUpdateStatus(obj, api.StatePending, &msg)
		status.Secrets.Processing = []api.CertificateSecretRef{}
		status.Secrets.Revoked = revokedSecrets
		status.Secrets.Failed = failedSecrets
		mod.Modified = true
		return mod, nil
	})
}

func (r *revokeReconciler) finishRevocation(logctx logger.LogContext, obj resources.Object, shouldRenewBeforeRevoke bool) reconcile.Status {
	revocation := obj.Data().(*api.CertificateRevocation)

	return r.updateStatusAndDelay(logctx, obj, 0, func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error) {
		state := api.StateRevocationApplied
		msg := "certificate(s) revoked"
		if shouldRenewBeforeRevoke {
			msg = "certificate renewed and old certificate(s) revoked"
		}
		if len(revocation.Status.Secrets.Revoked) > 0 {
			if len(revocation.Status.Secrets.Failed) > 0 || len(revocation.Status.Objects.Failed) > 0 {
				state = api.StateRevocationPartialApplied
				msg += " (ony partially successful)"
			}
		} else {
			msg = "revocation of certificate failed: " + *revocation.Status.Message
			state = api.StateError
		}
		mod, status := r.prepareUpdateStatus(obj, state, &msg)
		status.RevocationApplied = &metav1.Time{Time: time.Now()}
		mod.Modified = true
		return mod, nil
	})
}

func (r *revokeReconciler) setCertificateSecretRevoked(secret *corev1.Secret) error {
	if !resources.SetAnnotation(secret, certificate.AnnotationRevoked, "true") {
		return nil
	}
	_, err := r.certSecretResources.Update(secret)
	return err
}

func (r *revokeReconciler) setCertificateSecretsRevokedBySerialNumbers(hashKey string, serialNumbers []*big.Int) error {
	list, err := certificate.FindAllCertificateSecretsByHashLabel(r.certSecretResources, hashKey)
	if err != nil {
		return err
	}

	var errs []error
	for _, item := range list {
		secret := item.Data().(*corev1.Secret)
		if _, ok := resources.GetAnnotation(secret, certificate.AnnotationRevoked); ok {
			continue
		}
		cert, err := legobridge.DecodeCertificateFromSecretData(secret.Data)
		if err != nil || cert.SerialNumber == nil {
			errs = append(errs, err)
			continue
		}
		for _, sn := range serialNumbers {
			if sn.Cmp(cert.SerialNumber) == 0 {
				err = r.setCertificateSecretRevoked(secret)
				if err != nil {
					errs = append(errs, err)
				}
				break
			}
		}
	}

	return err
}

func (r *revokeReconciler) loadSecret(secretRef *corev1.SecretReference) (*corev1.Secret, error) {
	secretObjectName := resources.NewObjectName(secretRef.Namespace, secretRef.Name)
	secret := &corev1.Secret{}
	_, err := r.certSecretResources.GetInto(secretObjectName, secret)
	if err != nil {
		return nil, errors.Wrap(err, "fetching secret failed")
	}

	return secret, nil
}

func (r *revokeReconciler) updateStatusAndDelay(logctx logger.LogContext, obj resources.Object, delay time.Duration,
	statusUpdater func(logctx logger.LogContext, obj resources.Object) (*resources.ModificationState, error)) reconcile.Status {
	mod, err := statusUpdater(logctx, obj)
	if err != nil {
		return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "statusUpdater"))
	}
	if mod.Modified {
		err := mod.UpdateStatus()
		if err != nil {
			return r.failedStop(logctx, obj, api.StateError, errors.Wrap(err, "UpdateStatus failed"))
		}
	}

	return reconcile.Recheck(logctx, nil, delay)
}

func (r *revokeReconciler) prepareUpdateStatus(obj resources.Object, state string, msg *string) (*resources.ModificationState, *api.CertificateRevocationStatus) {
	revocation := obj.Data().(*api.CertificateRevocation)
	status := &revocation.Status

	mod := resources.NewModificationState(obj)
	mod.AssureStringPtrPtr(&status.Message, msg)
	mod.AssureStringValue(&status.State, state)
	mod.AssureInt64Value(&status.ObservedGeneration, obj.GetGeneration())
	return mod, status
}

func (r *revokeReconciler) failed(logctx logger.LogContext, obj resources.Object, state string, err error) reconcile.Status {
	return r.status(logctx, obj, state, err, false)
}

func (r *revokeReconciler) failedStop(logctx logger.LogContext, obj resources.Object, state string, err error) reconcile.Status {
	return r.status(logctx, obj, state, err, true)
}

func (r *revokeReconciler) status(logctx logger.LogContext, obj resources.Object, state string, err error, stop bool) reconcile.Status {
	msg := err.Error()

	rerr, isRecoverable := err.(*recoverableError)
	mod, _ := r.prepareUpdateStatus(obj, state, &msg)
	err2 := mod.UpdateStatus()
	if err2 != nil {
		logctx.Warnf("updating status failed with: %s", err2)
	}

	if isRecoverable {
		if rerr.Interval != 0 {
			return reconcile.Recheck(logctx, err, rerr.Interval)
		}
		return reconcile.Delay(logctx, err)
	}
	return reconcile.Failed(logctx, err)
}

func (r *revokeReconciler) recheck(logger logger.LogContext, obj resources.Object, state string, err error, interval time.Duration) reconcile.Status {
	return r.status(logger, obj, state, &recoverableError{Msg: err.Error(), Interval: interval}, false)
}

func createLabelCertificateHashKeySelector(hash string) (labels.Selector, error) {
	requirement, err := labels.NewRequirement(certificate.LabelCertificateHashKey, selection.Equals, []string{hash})
	if err != nil {
		return nil, err
	}
	return labels.NewSelector().Add(*requirement), nil
}

func isInvolved(revocation *api.CertificateRevocation, cert *api.Certificate) bool {
	if revocation.Status.Objects != nil {
		for _, ref := range revocation.Status.Objects.Processing {
			if ref.Namespace == cert.Namespace && ref.Name == ref.Name {
				return true
			}
		}
		for _, ref := range revocation.Status.Objects.Revoked {
			if ref.Namespace == cert.Namespace && ref.Name == ref.Name {
				return true
			}
		}
		for _, ref := range revocation.Status.Objects.Failed {
			if ref.Namespace == cert.Namespace && ref.Name == ref.Name {
				return true
			}
		}
	}
	return false
}
