package controller_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/martin31821/eks-webhack/internal/controller"
)

var _ = Describe("EndpointProxyReconciler", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "eptest-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	Describe("translating a NodePort clone EndpointSlice", func() {
		const (
			originalSvcName = "my-webhook"
			nodeIP          = "10.0.1.100"
			podIP           = "192.168.1.5"
		)

		var (
			node     *corev1.Node
			cloneSvc *corev1.Service
			srcEPS   *discoveryv1.EndpointSlice
		)

		BeforeEach(func() {
			nodeName := "worker-" + ns.Name

			// Create a node with a known internal IP.
			node = &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: nodeIP},
			}
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })

			cloneName := controller.CloneServiceName(originalSvcName)

			// Simulate the NodePort clone that ServiceReconciler would create.
			// Do NOT specify NodePort — let the API server assign it to avoid conflicts
			// across test runs when namespace deletion is asynchronous.
			cloneSvc = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cloneName,
					Namespace: ns.Name,
					Labels: map[string]string{
						controller.LabelManagedBy:       controller.LabelManagedByValue,
						controller.LabelOriginalService: originalSvcName,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeNodePort,
					Selector: map[string]string{"app": "my-webhook"},
					Ports: []corev1.ServicePort{{
						Name:       "https",
						Port:       443,
						Protocol:   corev1.ProtocolTCP,
						TargetPort: intstr.FromInt32(9443),
					}},
				},
			}
			Expect(k8sClient.Create(ctx, cloneSvc)).To(Succeed())

			// Wait for the API server to assign a NodePort.
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cloneName}, cloneSvc)
				if len(cloneSvc.Spec.Ports) > 0 {
					return cloneSvc.Spec.Ports[0].NodePort
				}
				return 0
			}, timeout, interval).Should(BeNumerically(">", 0))

			// Simulate the EndpointSlice that Kubernetes would create for the clone.
			srcEPS = &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: cloneName + "-",
					Namespace:    ns.Name,
					Labels: map[string]string{
						discoveryv1.LabelServiceName: cloneName,
					},
				},
				AddressType: discoveryv1.AddressTypeIPv4,
				Endpoints: []discoveryv1.Endpoint{{
					Addresses:  []string{podIP},
					NodeName:   ptr.To(nodeName),
					Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
				}},
				Ports: []discoveryv1.EndpointPort{{
					Name:     ptr.To("https"),
					Port:     ptr.To(int32(9443)),
					Protocol: protocolTCP(),
				}},
			}
			Expect(k8sClient.Create(ctx, srcEPS)).To(Succeed())
		})

		It("creates a managed EndpointSlice on the original service with node IPs and NodePort", func() {
			managedName := controller.ManagedEndpointSliceName(srcEPS.Name)

			var managedEPS discoveryv1.EndpointSlice
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &managedEPS)
			}, timeout, interval).Should(Succeed())

			Expect(managedEPS.Labels[discoveryv1.LabelServiceName]).To(Equal(originalSvcName))
			Expect(managedEPS.Labels[discoveryv1.LabelManagedBy]).To(Equal(controller.LabelEPSManagedByValue))
			Expect(managedEPS.Labels[controller.LabelManagedBy]).To(Equal(controller.LabelManagedByValue))
			Expect(managedEPS.Labels[controller.LabelSourceService]).To(Equal(cloneSvc.Name))

			Expect(managedEPS.Endpoints).To(HaveLen(1))
			Expect(managedEPS.Endpoints[0].Addresses).To(ConsistOf(nodeIP))
			Expect(managedEPS.Endpoints[0].Conditions.Ready).To(HaveValue(BeTrue()))

			Expect(managedEPS.Ports).To(HaveLen(1))
			Expect(*managedEPS.Ports[0].Port).To(Equal(cloneSvc.Spec.Ports[0].NodePort))
		})

		It("updates the managed EndpointSlice when the source EndpointSlice gains a new endpoint", func() {
			const secondNodeIP = "10.0.1.101"
			secondNodeName := "worker2-" + ns.Name

			managedName := controller.ManagedEndpointSliceName(srcEPS.Name)

			// Wait for the initial managed EPS.
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &discoveryv1.EndpointSlice{})
			}, timeout, interval).Should(Succeed())

			node2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: secondNodeName}}
			Expect(k8sClient.Create(ctx, node2)).To(Succeed())
			node2.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: secondNodeIP},
			}
			Expect(k8sClient.Status().Update(ctx, node2)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, node2) })

			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: srcEPS.Name}, srcEPS)).To(Succeed())
			srcEPS.Endpoints = append(srcEPS.Endpoints, discoveryv1.Endpoint{
				Addresses:  []string{"192.168.1.6"},
				NodeName:   ptr.To(secondNodeName),
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			})
			Expect(k8sClient.Update(ctx, srcEPS)).To(Succeed())

			Eventually(func() int {
				var managed discoveryv1.EndpointSlice
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &managed)
				return len(managed.Endpoints)
			}, timeout, interval).Should(Equal(2))
		})

		It("deduplicates endpoints from the same node into a single entry", func() {
			managedName := controller.ManagedEndpointSliceName(srcEPS.Name)

			// Wait for initial managed EPS (1 endpoint).
			Eventually(func() int {
				var managed discoveryv1.EndpointSlice
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &managed)
				return len(managed.Endpoints)
			}, timeout, interval).Should(Equal(1))

			// Add a second pod on the same node.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: srcEPS.Name}, srcEPS)).To(Succeed())
			srcEPS.Endpoints = append(srcEPS.Endpoints, discoveryv1.Endpoint{
				Addresses:  []string{"192.168.1.7"},
				NodeName:   ptr.To(node.Name), // same node
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			})
			Expect(k8sClient.Update(ctx, srcEPS)).To(Succeed())

			// Should still be 1 endpoint (deduplicated to one node IP).
			Consistently(func() int {
				var managed discoveryv1.EndpointSlice
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &managed)
				return len(managed.Endpoints)
			}, 2*timeout/10, interval).Should(Equal(1))
		})

		It("deletes the managed EndpointSlice when the clone service is deleted", func() {
			managedName := controller.ManagedEndpointSliceName(srcEPS.Name)

			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &discoveryv1.EndpointSlice{})
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, cloneSvc)).To(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: managedName}, &discoveryv1.EndpointSlice{})
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})

func protocolTCP() *corev1.Protocol {
	p := corev1.ProtocolTCP
	return &p
}
