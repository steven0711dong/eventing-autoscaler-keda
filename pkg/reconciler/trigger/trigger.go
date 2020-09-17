/*
Copyright 2020 The Knative Authors

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

package trigger

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/logging"

	"knative.dev/eventing-autoscaler-keda/pkg/reconciler/trigger/resources"
	"knative.dev/eventing/pkg/apis/eventing"
	v1 "knative.dev/eventing/pkg/apis/eventing/v1"
	triggerreconciler "knative.dev/eventing/pkg/client/injection/reconciler/eventing/v1/trigger"
	eventinglisters "knative.dev/eventing/pkg/client/listers/eventing/v1"

	pkgreconciler "knative.dev/pkg/reconciler"

	kedaclientset "github.com/kedacore/keda/pkg/generated/clientset/versioned"
	kedalisters "github.com/kedacore/keda/pkg/generated/listers/keda/v1alpha1"
)

const (
	// Name of the corev1.Events emitted from the Trigger reconciliation process.
	triggerReconciled = "TriggerReconciled"
)

// This has to stay in sync with:
// https://github.com/knative-sandbox/eventing-rabbitmq/blob/master/pkg/reconciler/broker/resources/secret.go#L49
func secretName(brokerName string) string {
	return fmt.Sprintf("%s-broker-rabbit", brokerName)
}

type Reconciler struct {
	kedaClientset kedaclientset.Interface

	// listers index properties about resources
	brokerLister       eventinglisters.BrokerLister
	scaledObjectLister kedalisters.ScaledObjectLister
	brokerClass        string
}

// Check that our Reconciler implements Interface
var _ triggerreconciler.Interface = (*Reconciler)(nil)
var _ triggerreconciler.Finalizer = (*Reconciler)(nil)

// ReconcilerArgs are the arguments needed to create a broker.Reconciler.
type ReconcilerArgs struct {
	DispatcherImage              string
	DispatcherServiceAccountName string
}

func newReconciledNormal(namespace, name string) pkgreconciler.Event {
	return pkgreconciler.NewEvent(corev1.EventTypeNormal, triggerReconciled, "Trigger reconciled: \"%s/%s\"", namespace, name)
}

func (r *Reconciler) ReconcileKind(ctx context.Context, t *v1.Trigger) pkgreconciler.Event {
	logging.FromContext(ctx).Debug("Reconciling", zap.Any("Trigger", t))

	broker, err := r.brokerLister.Brokers(t.Namespace).Get(t.Spec.Broker)
	if err != nil {
		if apierrs.IsNotFound(err) {
			// Ok to return nil here. Once the Broker comes available, or Trigger changes, we get requeued.
			return nil
		}
		logging.FromContext(ctx).Errorf("Failed to get Broker: \"%s/%s\" : %s", t.Spec.Broker, t.Namespace, err)
		return nil
	}

	// If it's not my brokerclass, ignore
	if broker.Annotations[eventing.BrokerClassKey] != r.brokerClass {
		logging.FromContext(ctx).Infof("Ignoring trigger %s/%s", t.Namespace, t.Name)
		return nil
	}

	// TODO: Check if there are any KEDA annotations before proceeding...
	// If they get updated / deleted, need to clean up.
	return r.reconcileScaledObject(ctx, t)
}

func (r *Reconciler) FinalizeKind(ctx context.Context, t *v1.Trigger) pkgreconciler.Event {
	// nil
	return nil
}

func (r *Reconciler) reconcileScaledObject(ctx context.Context, trigger *v1.Trigger) error {
	so := resources.MakeDispatcherScaledObject(ctx, trigger)

	current, err := r.scaledObjectLister.ScaledObjects(so.Namespace).Get(so.Name)
	if apierrs.IsNotFound(err) {
		_, err = r.kedaClientset.KedaV1alpha1().ScaledObjects(so.Namespace).Create(ctx, so, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepDerivative(so.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = so.Spec
		_, err = r.kedaClientset.KedaV1alpha1().ScaledObjects(desired.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
		return err
	}
	return nil
}