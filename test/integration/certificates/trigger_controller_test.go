/*
Copyright 2020 The Jetstack cert-manager contributors.

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

package certificates

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/clock"

	apiutil "github.com/jetstack/cert-manager/pkg/api/util"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	controllerpkg "github.com/jetstack/cert-manager/pkg/controller"
	"github.com/jetstack/cert-manager/pkg/controller/expcertificates/trigger"
	"github.com/jetstack/cert-manager/pkg/controller/expcertificates/trigger/policies"
	logf "github.com/jetstack/cert-manager/pkg/logs"
	"github.com/jetstack/cert-manager/pkg/metrics"
	"github.com/jetstack/cert-manager/test/integration/framework"
)

// TestTriggerController performs a basic test to ensure that the trigger
// controller works when instantiated.
// This is not an exhaustive set of test cases. It only ensures that an
// issuance is triggered when a new Certificate resource is created and
// no Secret exists.
func TestTriggerController(t *testing.T) {
	config, stopFn := framework.RunControlPlane(t)
	defer stopFn()

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*20)
	defer cancel()

	// Build, instantiate and run the trigger controller.
	_, factory, cmCl, cmFactory := framework.NewClients(t, config)
	ctrl, queue, mustSync := trigger.NewController(logf.Log, cmCl, factory, cmFactory, framework.NewEventRecorder(t), clock.RealClock{}, policies.TriggerPolicyChain)
	c := controllerpkg.NewController(
		context.Background(),
		"trigger_test",
		metrics.New(logf.Log),
		ctrl.ProcessItem,
		mustSync,
		nil,
		queue,
	)
	stopController := framework.StartInformersAndController(t, factory, cmFactory, c)
	defer stopController()

	// Create a Certificate resource and wait for it to have the 'Issuing' condition.
	cert, err := cmCl.CertmanagerV1alpha2().Certificates("testns").Create(ctx, &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "testcrt", Namespace: "testns"},
		Spec: cmapi.CertificateSpec{
			SecretName: "example",
			CommonName: "example.com",
			IssuerRef:  cmmeta.ObjectReference{Name: "testissuer"}, // doesn't need to exist
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = wait.Poll(time.Millisecond*100, time.Second*5, func() (done bool, err error) {
		c, err := cmCl.CertmanagerV1alpha2().Certificates(cert.Namespace).Get(ctx, cert.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Failed to fetch Certificate resource, retrying: %v", err)
			return false, nil
		}
		if !apiutil.CertificateHasCondition(c, cmapi.CertificateCondition{
			Type:   cmapi.CertificateConditionIssuing,
			Status: cmmeta.ConditionTrue,
		}) {
			t.Logf("Certificate does not have expected condition, got=%#v", apiutil.GetCertificateCondition(c, cmapi.CertificateConditionIssuing))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
