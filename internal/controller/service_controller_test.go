package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/martin31821/eks-webhack/internal/controller"
)

const (
	timeout  = 10 * time.Second
	interval = 100 * time.Millisecond
)

var _ = Describe("ServiceReconciler", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	Describe("ClusterIP service referenced by a ValidatingWebhookConfiguration", func() {
		var (
			svc  *corev1.Service
			vwh  *admissionv1.ValidatingWebhookConfiguration
			port int32 = 443
		)

		BeforeEach(func() {
			svc = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-webhook",
					Namespace: ns.Name,
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app": "my-webhook",
					},
					Ports: []corev1.ServicePort{{
						Name:       "https",
						Port:       443,
						Protocol:   corev1.ProtocolTCP,
						TargetPort: intstr.FromInt32(9443),
					}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			vwh = &admissionv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "test-vwh-" + ns.Name},
				Webhooks: []admissionv1.ValidatingWebhook{{
					Name:                    "validate.test.io",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             sideEffectsNone(),
					ClientConfig: admissionv1.WebhookClientConfig{
						Service: &admissionv1.ServiceReference{
							Namespace: ns.Name,
							Name:      "my-webhook",
							Port:      &port,
						},
					},
				}},
			}
			Expect(k8sClient.Create(ctx, vwh)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, vwh)
		})

		It("creates a NodePort clone with the original selector", func() {
			cloneName := controller.CloneServiceName("my-webhook")
			clone := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, clone)
			}, timeout, interval).Should(Succeed())

			Expect(clone.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
			Expect(clone.Spec.Selector).To(HaveKeyWithValue("app", "my-webhook"))
			Expect(clone.Spec.Selector).NotTo(HaveKey(controller.LabelExcludeFromEndpointsKey))
			Expect(clone.Labels).To(HaveKeyWithValue(controller.LabelManagedBy, controller.LabelManagedByValue))
			Expect(clone.Labels).To(HaveKeyWithValue(controller.LabelOriginalService, "my-webhook"))
		})

		It("injects the exclude-from-endpoints label into the original service selector only after a managed EndpointSlice exists", func() {
			cloneName := controller.CloneServiceName("my-webhook")

			// Wait for the clone to appear — proves ServiceReconciler has run steps 1 & 2.
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, &corev1.Service{})
			}, timeout, interval).Should(Succeed())

			// Selector must NOT be poisoned yet (no managed EPS exists).
			Consistently(func() string {
				svc := &corev1.Service{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "my-webhook"}, svc)
				return svc.Spec.Selector[controller.LabelExcludeFromEndpointsKey]
			}, timeout/5, interval).Should(BeEmpty())

			// Simulate EndpointProxyReconciler publishing a managed EndpointSlice.
			managedEPS := &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "managed-test-",
					Namespace:    ns.Name,
					Labels: map[string]string{
						discoveryv1.LabelServiceName: "my-webhook",
						discoveryv1.LabelManagedBy:   controller.LabelEPSManagedByValue,
						controller.LabelManagedBy:    controller.LabelManagedByValue,
					},
				},
				AddressType: discoveryv1.AddressTypeIPv4,
				Endpoints:   []discoveryv1.Endpoint{},
				Ports:       []discoveryv1.EndpointPort{},
			}
			Expect(k8sClient.Create(ctx, managedEPS)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, managedEPS) })

			// Now the selector should be patched.
			updated := &corev1.Service{}
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "my-webhook"}, updated)
				return updated.Spec.Selector[controller.LabelExcludeFromEndpointsKey] == controller.LabelExcludeFromEndpointsValue
			}, timeout, interval).Should(BeTrue())

			Expect(updated.Spec.Selector).To(HaveKeyWithValue("app", "my-webhook"))
		})

		It("sets an OwnerReference from the clone to the original service", func() {
			cloneName := controller.CloneServiceName("my-webhook")
			clone := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, clone)
			}, timeout, interval).Should(Succeed())

			Expect(clone.OwnerReferences).To(HaveLen(1))
			Expect(clone.OwnerReferences[0].Name).To(Equal("my-webhook"))
		})

		It("restores the original selector and removes the clone when the webhook config is deleted", func() {
			cloneName := controller.CloneServiceName("my-webhook")

			// Wait for clone to be created first.
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, &corev1.Service{})
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, vwh)).To(Succeed())

			// Clone should be deleted.
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, &corev1.Service{})
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())

			// Selector should be restored.
			restored := &corev1.Service{}
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "my-webhook"}, restored)
				_, hasExclude := restored.Spec.Selector[controller.LabelExcludeFromEndpointsKey]
				return !hasExclude
			}, timeout, interval).Should(BeTrue())
		})
	})

	Describe("non-ClusterIP service referenced by a webhook", func() {
		It("does not create a clone for a NodePort service", func() {
			port := int32(443)
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "np-webhook", Namespace: ns.Name},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeNodePort,
					Selector: map[string]string{"app": "np-webhook"},
					Ports: []corev1.ServicePort{{
						Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(9443),
					}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			vwh := &admissionv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "test-np-vwh-" + ns.Name},
				Webhooks: []admissionv1.ValidatingWebhook{{
					Name:                    "validate.np.test.io",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             sideEffectsNone(),
					ClientConfig: admissionv1.WebhookClientConfig{
						Service: &admissionv1.ServiceReference{
							Namespace: ns.Name, Name: "np-webhook", Port: &port,
						},
					},
				}},
			}
			Expect(k8sClient.Create(ctx, vwh)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vwh) })

			cloneName := controller.CloneServiceName("np-webhook")
			Consistently(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, &corev1.Service{})
				return apierrors.IsNotFound(err)
			}, 2*time.Second, interval).Should(BeTrue())
		})
	})

	Describe("service not referenced by any webhook", func() {
		It("ignores it entirely", func() {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "plain-svc", Namespace: ns.Name},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeClusterIP,
					Selector: map[string]string{"app": "plain"},
					Ports: []corev1.ServicePort{{
						Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(8080),
					}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			cloneName := controller.CloneServiceName("plain-svc")
			Consistently(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, &corev1.Service{})
				return apierrors.IsNotFound(err)
			}, 2*time.Second, interval).Should(BeTrue())
		})
	})
})

func sideEffectsNone() *admissionv1.SideEffectClass {
	v := admissionv1.SideEffectClassNone
	return &v
}
