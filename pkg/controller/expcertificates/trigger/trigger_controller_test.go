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

package trigger

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coretesting "k8s.io/client-go/testing"
	fakeclock "k8s.io/utils/clock/testing"

	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	controllerpkg "github.com/jetstack/cert-manager/pkg/controller"
	"github.com/jetstack/cert-manager/pkg/controller/expcertificates/trigger/policies"
	testpkg "github.com/jetstack/cert-manager/pkg/controller/test"
)

// policyFuncBuilder wraps a policies.Func to allow injecting a testing.T
type policyFuncBuilder func(t *testing.T) policies.Func

func TestProcessItem(t *testing.T) {
	// now time is the current time at the start of the test (the clock is fixed)
	now := time.Now()
	metaNow := metav1.NewTime(now)
	forceTriggeredReason := "ForceTriggered"
	forceTriggeredMessage := "Re-issuance forced by unit test case"
	tests := map[string]struct {
		// key that should be passed to ProcessItem.
		// if not set, the 'namespace/name' of the 'Certificate' field will be used.
		// if neither is set, the key will be ""
		key string

		// Certificate to be synced for the test.
		// if not set, the 'key' will be passed to ProcessItem instead.
		certificate *cmapi.Certificate

		// Secret, if set, will exist in the apiserver before the test is run.
		secret *corev1.Secret

		// Request, if set, will exist in the apiserver before the test is run.
		requests []*cmapi.CertificateRequest

		// optional chain of policy functions that should be run, wrapped with
		// the policyFuncBuilder to allow injecting the sub-test's testing.T.
		policyFuncs []policyFuncBuilder

		// chainShouldEvaluate will cause the test to error if the policy chain
		// was not attempted to be evaluated
		chainShouldEvaluate bool
		// chainShouldTriggerIssuance will cause the policy chain used in the
		// test to trigger issuance.
		// This policyFunc will be injected at the end of the policy chain.
		// If false, the policyFunc that forces an issuance will not be injected
		// but user-provided policyFuncs will still behave as usual.
		chainShouldTriggerIssuance bool

		// expectedEvent, if set, is an 'event string' that is expected to be fired.
		// For example, "Normal Issuing Re-issuance forced by unit test case"
		// where 'Normal' is the event severity, 'Issuing' is the reason and the
		// remainder is the message.
		expectedEvent string

		// expectedConditions is the expected set of conditions on the Certificate
		// resource if an Update is made.
		// If nil, no update is expected.
		// If empty, an update to the empty set/nil is expected.
		expectedConditions []cmapi.CertificateCondition

		// err is the expected error text returned by the controller, if any.
		err string
	}{
		"do nothing if an empty 'key' is used": {},
		"do nothing if an invalid 'key' is used": {
			key: "abc/def/ghi",
		},
		"do nothing if a key references a Certificate that does not exist": {
			key: "namespace/name",
		},
		"do nothing if Certificate already has 'Issuing' condition": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Status: cmapi.CertificateStatus{
					Conditions: []cmapi.CertificateCondition{
						{
							Type:   cmapi.CertificateConditionIssuing,
							Status: cmmeta.ConditionTrue,
						},
					},
				},
			},
		},
		"evaluate policy chain with only the Certificate if no Request or Secret exists": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
			},
			chainShouldEvaluate: true,
			policyFuncs: []policyFuncBuilder{
				// Add a policy function that ensures only the input's 'certificate'
				// field is set.
				func(t *testing.T) policies.Func {
					return func(input policies.Input) (string, string, bool) {
						if input.Certificate == nil {
							t.Error("expected policy data 'Certificate' field to be set but it was not")
						}
						if input.Secret != nil {
							t.Errorf("expected policy data 'Secret' field to be unset but it was: %+v", input.Secret)
						}
						if input.CurrentRevisionRequest != nil {
							t.Errorf("expected policy data 'CurrentRevisionRequest' field to be unset but it was: %+v", input.CurrentRevisionRequest)
						}
						return "", "", false
					}
				},
			},
		},
		"evaluate policy chain with the Certificate and Secret if no Request exists": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Spec: cmapi.CertificateSpec{
					SecretName: "test-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test-secret"},
			},
			chainShouldEvaluate: true,
			policyFuncs: []policyFuncBuilder{
				// Add a policy function that ensures only the input's 'certificate'
				// field is set.
				func(t *testing.T) policies.Func {
					return func(input policies.Input) (string, string, bool) {
						if input.Certificate == nil {
							t.Error("expected policy data 'Certificate' field to be set but it was not")
						}
						if input.Secret == nil {
							t.Errorf("expected policy data 'Secret' field to be set but it was not")
						}
						if input.CurrentRevisionRequest != nil {
							t.Errorf("expected policy data 'CurrentRevisionRequest' field to be unset but it was: %+v", input.CurrentRevisionRequest)
						}
						return "", "", false
					}
				},
			},
		},
		"evaluate policy chain with the Certificate, Secret and Request if one exists": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Spec: cmapi.CertificateSpec{
					SecretName: "test-secret",
				},
				Status: cmapi.CertificateStatus{
					Revision: func(i int) *int { return &i }(3),
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test-secret"},
			},
			requests: []*cmapi.CertificateRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "testns",
						Name:      "test",
						Annotations: map[string]string{
							cmapi.CertificateRequestRevisionAnnotationKey: "3",
						},
						OwnerReferences: []metav1.OwnerReference{
							*metav1.NewControllerRef(&cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"}}, cmapi.SchemeGroupVersion.WithKind("Certificate")),
						},
					},
				},
			},
			chainShouldEvaluate: true,
			policyFuncs: []policyFuncBuilder{
				// Add a policy function that ensures only the input's 'certificate'
				// field is set.
				func(t *testing.T) policies.Func {
					return func(input policies.Input) (string, string, bool) {
						if input.Certificate == nil {
							t.Error("expected policy data 'Certificate' field to be set but it was not")
						}
						if input.Secret == nil {
							t.Errorf("expected policy data 'Secret' field to be set but it was not")
						}
						if input.CurrentRevisionRequest == nil {
							t.Errorf("expected policy data 'CurrentRevisionRequest' field to be set but it was not")
						}
						return "", "", false
					}
				},
			},
		},
		"error if multiple owned CertificateRequest resources exist and have the same revision": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Spec: cmapi.CertificateSpec{
					SecretName: "test-secret",
				},
				Status: cmapi.CertificateStatus{
					Revision: func(i int) *int { return &i }(3),
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test-secret"},
			},
			requests: []*cmapi.CertificateRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "testns",
						Name:      "test",
						Annotations: map[string]string{
							cmapi.CertificateRequestRevisionAnnotationKey: "3",
						},
						OwnerReferences: []metav1.OwnerReference{
							*metav1.NewControllerRef(&cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"}}, cmapi.SchemeGroupVersion.WithKind("Certificate")),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "testns",
						Name:      "test-number-two",
						Annotations: map[string]string{
							cmapi.CertificateRequestRevisionAnnotationKey: "3",
						},
						OwnerReferences: []metav1.OwnerReference{
							*metav1.NewControllerRef(&cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"}}, cmapi.SchemeGroupVersion.WithKind("Certificate")),
						},
					},
				},
			},
			chainShouldEvaluate: false,
			err:                 "multiple CertificateRequest resources exist for the current revision, not triggering new issuance until requests have been cleaned up",
		},
		"should evaluate policy if no certificaterequest resource exists for the current revision": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Spec: cmapi.CertificateSpec{
					SecretName: "test-secret",
				},
				Status: cmapi.CertificateStatus{
					Revision: func(i int) *int { return &i }(3),
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test-secret"},
			},
			chainShouldEvaluate: true,
		},
		"should set the 'Issuing' status condition if the chain indicates an issuance is required": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
			},
			chainShouldEvaluate:        true,
			chainShouldTriggerIssuance: true,
			expectedEvent:              "Normal Issuing Re-issuance forced by unit test case",
			expectedConditions: []cmapi.CertificateCondition{
				{
					Type:               cmapi.CertificateConditionIssuing,
					Status:             cmmeta.ConditionTrue,
					Reason:             forceTriggeredReason,
					Message:            forceTriggeredMessage,
					LastTransitionTime: &metaNow,
				},
			},
		},
		"should not set the 'Issuing' status condition if the chain indicates an issuance is required if the last failure time is within the last hour": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Status: cmapi.CertificateStatus{
					LastFailureTime: func(m metav1.Time) *metav1.Time { return &m }(metav1.NewTime(now.Add(-59 * time.Minute))),
				},
			},
			chainShouldEvaluate:        true,
			chainShouldTriggerIssuance: true,
		},
		"should set the 'Issuing' status condition if the chain indicates an issuance is required if the last failure time is older than the last hour": {
			certificate: &cmapi.Certificate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "test"},
				Status: cmapi.CertificateStatus{
					LastFailureTime: func(m metav1.Time) *metav1.Time { return &m }(metav1.NewTime(now.Add(-61 * time.Minute))),
				},
			},
			chainShouldEvaluate:        true,
			chainShouldTriggerIssuance: true,
			expectedEvent:              "Normal Issuing Re-issuance forced by unit test case",
			expectedConditions: []cmapi.CertificateCondition{
				{
					Type:               cmapi.CertificateConditionIssuing,
					Status:             cmmeta.ConditionTrue,
					Reason:             forceTriggeredReason,
					Message:            forceTriggeredMessage,
					LastTransitionTime: &metaNow,
				},
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			// Create and initialise a new unit test builder
			builder := &testpkg.Builder{
				T:     t,
				Clock: fakeclock.NewFakeClock(now),
			}
			if test.certificate != nil {
				builder.CertManagerObjects = append(builder.CertManagerObjects, test.certificate)
			}
			if test.secret != nil {
				builder.KubeObjects = append(builder.KubeObjects, test.secret)
			}
			for _, req := range test.requests {
				builder.CertManagerObjects = append(builder.CertManagerObjects, req)
			}
			builder.Init()

			// Register informers used by the controller using the registration wrapper
			w := &controllerWrapper{}
			_, _, err := w.Register(builder.Context)
			if err != nil {
				t.Fatal(err)
			}
			// Fake out the default policy chain
			w.policyChain = []policies.Func{}
			// Record whether the policy chain was evaluated
			evaluated := false
			w.policyChain = append(w.policyChain, func(_ policies.Input) (string, string, bool) {
				evaluated = true
				return "", "", false
			})
			// Add any test-specific policies to the chain
			w.policyChain = append(w.policyChain, buildTestPolicyChain(t, test.policyFuncs...)...)
			// If the chain should trigger an issuance, inject an 'always reissue'
			// policyFunc at the end of the chain
			if test.chainShouldTriggerIssuance {
				w.policyChain = append(w.policyChain, func(_ policies.Input) (string, string, bool) {
					return forceTriggeredReason, forceTriggeredMessage, true
				})
			}
			if test.expectedConditions != nil {
				if test.certificate == nil {
					t.Fatal("cannot expect an Update operation if test.certificate is nil")
				}
				expectedCert := test.certificate.DeepCopy()
				expectedCert.Status.Conditions = test.expectedConditions
				builder.ExpectedActions = append(builder.ExpectedActions,
					testpkg.NewAction(coretesting.NewUpdateSubresourceAction(
						cmapi.SchemeGroupVersion.WithResource("certificates"),
						"status",
						test.certificate.Namespace,
						expectedCert,
					)),
				)
			}
			if test.expectedEvent != "" {
				builder.ExpectedEvents = []string{test.expectedEvent}
			}
			// Start the informers and begin processing updates
			builder.Start()
			defer builder.Stop()

			key := test.key
			if key == "" && test.certificate != nil {
				key, err = controllerpkg.KeyFunc(test.certificate)
				if err != nil {
					t.Fatal(err)
				}
			}

			// Call ProcessItem
			err = w.controller.ProcessItem(context.Background(), key)
			switch {
			case err != nil:
				if test.err != err.Error() {
					t.Errorf("error text did not match, got=%s, exp=%s", err.Error(), test.err)
				}
			default:
				if test.err != "" {
					t.Errorf("got no error but expected: %s", test.err)
				}
			}
			if evaluated != test.chainShouldEvaluate {
				if test.chainShouldEvaluate {
					t.Error("expected policy chain to be evaluated but it was not")
				} else {
					t.Error("expected policy chain to NOT be evaluated but it was")
				}
			}

			if err := builder.AllEventsCalled(); err != nil {
				builder.T.Error(err)
			}
			if err := builder.AllActionsExecuted(); err != nil {
				builder.T.Error(err)
			}
			if err := builder.AllReactorsCalled(); err != nil {
				builder.T.Error(err)
			}
		})
	}
}

func buildTestPolicyChain(t *testing.T, funcs ...policyFuncBuilder) policies.Chain {
	c := policies.Chain{}
	for _, f := range funcs {
		c = append(c, f(t))
	}
	return c
}
