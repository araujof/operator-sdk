// Copyright 2018 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"time"

	rpb "helm.sh/helm/v3/pkg/release"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/operator-framework/operator-sdk/internal/util/diffutil"
	"github.com/operator-framework/operator-sdk/pkg/helm/internal/types"
	"github.com/operator-framework/operator-sdk/pkg/helm/release"
)

// blank assignment to verify that HelmOperatorReconciler implements reconcile.Reconciler
var _ reconcile.Reconciler = &HelmOperatorReconciler{}

// ReleaseHookFunc defines a function signature for release hooks.
type ReleaseHookFunc func(*rpb.Release) error

// HelmOperatorReconciler reconciles custom resources as Helm releases.
type HelmOperatorReconciler struct {
	Client          client.Client
	EventRecorder   record.EventRecorder
	GVK             schema.GroupVersionKind
	ManagerFactory  release.ManagerFactory
	ReconcilePeriod time.Duration
	OverrideValues  map[string]string
	releaseHook     ReleaseHookFunc
}

const (
	finalizer = "uninstall-helm-release"
)

// Reconcile reconciles the requested resource by installing, updating, or
// uninstalling a Helm release based on the resource's current state. If no
// release changes are necessary, Reconcile will create or patch the underlying
// resources to match the expected release manifest.
func (r HelmOperatorReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(r.GVK)
	o.SetNamespace(request.Namespace)
	o.SetName(request.Name)
	log := log.WithValues(
		"namespace", o.GetNamespace(),
		"name", o.GetName(),
		"apiVersion", o.GetAPIVersion(),
		"kind", o.GetKind(),
	)
	log.V(1).Info("Reconciling")

	err := r.Client.Get(context.TODO(), request.NamespacedName, o)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		log.Error(err, "Failed to lookup resource")
		return reconcile.Result{}, err
	}

	manager, err := r.ManagerFactory.NewManager(o, r.OverrideValues)
	if err != nil {
		log.Error(err, "Failed to get release manager")
		return reconcile.Result{}, err
	}

	status := types.StatusFor(o)
	log = log.WithValues("release", manager.ReleaseName())

	deleted := o.GetDeletionTimestamp() != nil
	pendingFinalizers := o.GetFinalizers()
	if !deleted && !contains(pendingFinalizers, finalizer) {
		log.V(1).Info("Adding finalizer", "finalizer", finalizer)
		finalizers := append(pendingFinalizers, finalizer)
		o.SetFinalizers(finalizers)
		err = r.updateResource(o)

		// Need to requeue because finalizer update does not change metadata.generation
		return reconcile.Result{Requeue: true}, err
	}

	status.SetCondition(types.HelmAppCondition{
		Type:   types.ConditionInitialized,
		Status: types.StatusTrue,
	})

	if err := manager.Sync(context.TODO()); err != nil {
		log.Error(err, "Failed to sync release")
		status.SetCondition(types.HelmAppCondition{
			Type:    types.ConditionIrreconcilable,
			Status:  types.StatusTrue,
			Reason:  types.ReasonReconcileError,
			Message: err.Error(),
		})
		_ = r.updateResourceStatus(o, status)
		return reconcile.Result{}, err
	}
	status.RemoveCondition(types.ConditionIrreconcilable)

	if deleted {
		if !contains(pendingFinalizers, finalizer) {
			log.Info("Resource is terminated, skipping reconciliation")
			return reconcile.Result{}, nil
		}

		uninstalledRelease, err := manager.UninstallRelease(context.TODO())
		if err != nil && err != release.ErrNotFound {
			log.Error(err, "Failed to uninstall release")
			status.SetCondition(types.HelmAppCondition{
				Type:    types.ConditionReleaseFailed,
				Status:  types.StatusTrue,
				Reason:  types.ReasonUninstallError,
				Message: err.Error(),
			})
			_ = r.updateResourceStatus(o, status)
			return reconcile.Result{}, err
		}
		status.RemoveCondition(types.ConditionReleaseFailed)

		if err == release.ErrNotFound {
			log.Info("Release not found, removing finalizer")
		} else {
			log.Info("Uninstalled release")
			if log.V(0).Enabled() {
				fmt.Println(diffutil.Diff(uninstalledRelease.Manifest, ""))
			}
			status.SetCondition(types.HelmAppCondition{
				Type:   types.ConditionDeployed,
				Status: types.StatusFalse,
				Reason: types.ReasonUninstallSuccessful,
			})
			status.DeployedRelease = nil
		}
		if err := r.updateResourceStatus(o, status); err != nil {
			return reconcile.Result{}, err
		}

		finalizers := []string{}
		for _, pendingFinalizer := range pendingFinalizers {
			if pendingFinalizer != finalizer {
				finalizers = append(finalizers, pendingFinalizer)
			}
		}
		o.SetFinalizers(finalizers)
		err = r.updateResource(o)

		// Need to requeue because finalizer update does not change metadata.generation
		return reconcile.Result{Requeue: true}, err
	}

	if !manager.IsInstalled() {
		for k, v := range r.OverrideValues {
			r.EventRecorder.Eventf(o, "Warning", "OverrideValuesInUse", "Chart value %q overridden to %q by operator's watches.yaml", k, v)
		}
		installedRelease, err := manager.InstallRelease(context.TODO())
		if err != nil {
			log.Error(err, "Release failed")
			status.SetCondition(types.HelmAppCondition{
				Type:    types.ConditionReleaseFailed,
				Status:  types.StatusTrue,
				Reason:  types.ReasonInstallError,
				Message: err.Error(),
			})
			_ = r.updateResourceStatus(o, status)
			return reconcile.Result{}, err
		}
		status.RemoveCondition(types.ConditionReleaseFailed)

		if r.releaseHook != nil {
			if err := r.releaseHook(installedRelease); err != nil {
				log.Error(err, "Failed to run release hook")
				return reconcile.Result{}, err
			}
		}

		log.Info("Installed release")
		if log.V(0).Enabled() {
			fmt.Println(diffutil.Diff("", installedRelease.Manifest))
		}
		log.V(1).Info("Config values", "values", installedRelease.Config)
		message := ""
		if installedRelease.Info != nil {
			message = installedRelease.Info.Notes
		}
		status.SetCondition(types.HelmAppCondition{
			Type:    types.ConditionDeployed,
			Status:  types.StatusTrue,
			Reason:  types.ReasonInstallSuccessful,
			Message: message,
		})
		status.DeployedRelease = &types.HelmAppRelease{
			Name:     installedRelease.Name,
			Manifest: installedRelease.Manifest,
		}
		err = r.updateResourceStatus(o, status)
		return reconcile.Result{RequeueAfter: r.ReconcilePeriod}, err
	}

	if manager.IsUpdateRequired() {
		for k, v := range r.OverrideValues {
			r.EventRecorder.Eventf(o, "Warning", "OverrideValuesInUse", "Chart value %q overridden to %q by operator's watches.yaml", k, v)
		}
		previousRelease, updatedRelease, err := manager.UpdateRelease(context.TODO())
		if err != nil {
			log.Error(err, "Release failed")
			status.SetCondition(types.HelmAppCondition{
				Type:    types.ConditionReleaseFailed,
				Status:  types.StatusTrue,
				Reason:  types.ReasonUpdateError,
				Message: err.Error(),
			})
			_ = r.updateResourceStatus(o, status)
			return reconcile.Result{}, err
		}
		status.RemoveCondition(types.ConditionReleaseFailed)

		if r.releaseHook != nil {
			if err := r.releaseHook(updatedRelease); err != nil {
				log.Error(err, "Failed to run release hook")
				return reconcile.Result{}, err
			}
		}

		log.Info("Updated release")
		if log.V(0).Enabled() {
			fmt.Println(diffutil.Diff(previousRelease.Manifest, updatedRelease.Manifest))
		}
		log.V(1).Info("Config values", "values", updatedRelease.Config)
		message := ""
		if updatedRelease.Info != nil {
			message = updatedRelease.Info.Notes
		}
		status.SetCondition(types.HelmAppCondition{
			Type:    types.ConditionDeployed,
			Status:  types.StatusTrue,
			Reason:  types.ReasonUpdateSuccessful,
			Message: message,
		})
		status.DeployedRelease = &types.HelmAppRelease{
			Name:     updatedRelease.Name,
			Manifest: updatedRelease.Manifest,
		}
		err = r.updateResourceStatus(o, status)
		return reconcile.Result{RequeueAfter: r.ReconcilePeriod}, err
	}

	// If a change is made to the CR spec that causes a release failure, a
	// ConditionReleaseFailed is added to the status conditions. If that change
	// is then reverted to its previous state, the operator will stop
	// attempting the release and will resume reconciling. In this case, we
	// need to remove the ConditionReleaseFailed because the failing release is
	// no longer being attempted.
	status.RemoveCondition(types.ConditionReleaseFailed)

	expectedRelease, err := manager.ReconcileRelease(context.TODO())
	if err != nil {
		log.Error(err, "Failed to reconcile release")
		status.SetCondition(types.HelmAppCondition{
			Type:    types.ConditionIrreconcilable,
			Status:  types.StatusTrue,
			Reason:  types.ReasonReconcileError,
			Message: err.Error(),
		})
		_ = r.updateResourceStatus(o, status)
		return reconcile.Result{}, err
	}
	status.RemoveCondition(types.ConditionIrreconcilable)

	if r.releaseHook != nil {
		if err := r.releaseHook(expectedRelease); err != nil {
			log.Error(err, "Failed to run release hook")
			return reconcile.Result{}, err
		}
	}

	log.Info("Reconciled release")
	status.DeployedRelease = &types.HelmAppRelease{
		Name:     expectedRelease.Name,
		Manifest: expectedRelease.Manifest,
	}
	err = r.updateResourceStatus(o, status)
	return reconcile.Result{RequeueAfter: r.ReconcilePeriod}, err
}

func (r HelmOperatorReconciler) updateResource(o runtime.Object) error {
	return r.Client.Update(context.TODO(), o)
}

func (r HelmOperatorReconciler) updateResourceStatus(o *unstructured.Unstructured, status *types.HelmAppStatus) error {
	o.Object["status"] = status
	return r.Client.Status().Update(context.TODO(), o)
}

func contains(l []string, s string) bool {
	for _, elem := range l {
		if elem == s {
			return true
		}
	}
	return false
}
