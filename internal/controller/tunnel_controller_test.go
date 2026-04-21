package controller

import (
	"context"

	"github.com/nais/tunnel-operator/pkg/portalloc"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
)

var _ = Describe("Tunnel Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const namespace = "default"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: namespace,
		}

		newRequest := func() ctrl.Request {
			return ctrl.Request{
				NamespacedName: typeNamespacedName,
			}
		}

		newReconciler := func() *TunnelReconciler {
			return &TunnelReconciler{
				Client:              k8sClient,
				Scheme:              k8sClient.Scheme(),
				PortAllocator:       portalloc.New(20000, 20010),
				ForwarderServiceKey: client.ObjectKey{Name: "test-forwarder", Namespace: namespace},
			}
		}

		BeforeEach(func() {
			ns := &corev1.Namespace{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating the custom resource for the Kind Tunnel")
			tunnel := &v1alpha1.Tunnel{}
			err = k8sClient.Get(ctx, typeNamespacedName, tunnel)
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha1.Tunnel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
					Spec: v1alpha1.TunnelSpec{
						TeamSlug:        "team-a",
						Environment:     "dev",
						ClientPublicKey: "client-public-key",
						Target: v1alpha1.TunnelTarget{
							Host:       "redis.example.internal",
							Port:       6379,
							ResolvedIP: "10.0.0.10",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterEach(func() {
			resource := &v1alpha1.Tunnel{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Removing finalizer and deleting the Tunnel resource")
			if controllerutil.ContainsFinalizer(resource, finalizerName) {
				controllerutil.RemoveFinalizer(resource, finalizerName)
				Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Cleaning up gateway resources")
			gwName := gatewayResourceName(resourceName)
			gwKey := types.NamespacedName{Name: gwName, Namespace: namespace}
			for _, obj := range []client.Object{
				&corev1.Pod{},
				&networkingv1.NetworkPolicy{},
			} {
				if err := k8sClient.Get(ctx, gwKey, obj); err == nil {
					_ = k8sClient.Delete(ctx, obj)
				}
			}
		})

		It("should create gateway resources and update status", func() {
			By("Reconciling the created resource")
			controllerReconciler := newReconciler()

			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, pod)).To(Succeed())
			Expect(pod.Labels).To(HaveKeyWithValue("tunnels.nais.io/tunnel", resourceName))
			Expect(pod.Spec.Containers[0].ReadinessProbe).NotTo(BeNil())
			Expect(pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path).To(Equal("/status"))

			networkPolicy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, networkPolicy)).To(Succeed())
			Expect(networkPolicy.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
			Expect(networkPolicy.Spec.Egress).To(HaveLen(2))
			Expect(networkPolicy.Spec.Egress[0].To).To(HaveLen(1))
			Expect(networkPolicy.Spec.Egress[0].To[0].IPBlock).NotTo(BeNil())
			Expect(networkPolicy.Spec.Egress[0].To[0].IPBlock.CIDR).To(Equal("10.0.0.10/32"))

			tunnel := &v1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tunnel)).To(Succeed())
			Expect(tunnel.Finalizers).To(ContainElement(finalizerName))
			Expect(tunnel.Status.Phase).To(Equal(v1alpha1.TunnelPhaseProvisioning))
			Expect(tunnel.Status.Message).To(Equal("Gateway pod starting"))
			Expect(tunnel.Status.GatewayPodName).To(Equal(gatewayResourceName(resourceName)))
			Expect(tunnel.Status.ForwarderPort).To(BeNumerically(">", 0))
		})

		It("should reconcile idempotently after resources already exist", func() {
			controllerReconciler := newReconciler()

			By("running two reconciles to create all resources")
			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			By("running a third reconcile with resources already present")
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, pod)).To(Succeed())

			tunnel := &v1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tunnel)).To(Succeed())
			Expect(tunnel.Status.Phase).To(Equal(v1alpha1.TunnelPhaseProvisioning))
			Expect(tunnel.Status.ForwarderPort).To(BeNumerically(">", 0))
		})

		It("should set correct env vars on the gateway pod", func() {
			controllerReconciler := newReconciler()

			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, pod)).To(Succeed())
			Expect(pod.Spec.Containers).To(HaveLen(1))

			envVars := pod.Spec.Containers[0].Env
			envByName := map[string]string{}
			for _, e := range envVars {
				envByName[e.Name] = e.Value
			}

			Expect(envByName).To(HaveKeyWithValue("TUNNEL_PEER_PUBLIC_KEY", "client-public-key"))
			Expect(envByName).To(HaveKeyWithValue("TUNNEL_TARGET_HOST", "redis.example.internal"))
			Expect(envByName).To(HaveKeyWithValue("TUNNEL_TARGET_PORT", "6379"))
			Expect(envByName).To(HaveKeyWithValue("TUNNEL_NAME", resourceName))
			Expect(envByName).To(HaveKeyWithValue("TUNNEL_NAMESPACE", namespace))
			Expect(envByName).To(HaveKey("LOG_LEVEL"))
		})

		It("should delete the tunnel CR when status is Terminated", func() {
			controllerReconciler := newReconciler()

			By("running the first reconcile to add the finalizer")
			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			By("setting tunnel status to Terminated")
			tunnel := &v1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tunnel)).To(Succeed())
			tunnel.Status.Phase = v1alpha1.TunnelPhaseTerminated
			Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())

			By("reconciling again to trigger CR deletion")
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			remaining := &v1alpha1.Tunnel{}
			err = k8sClient.Get(ctx, typeNamespacedName, remaining)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should respect custom activeDeadlineSeconds on the gateway pod", func() {
			controllerReconciler := newReconciler()

			By("updating the tunnel spec with a custom deadline before reconciling")
			tunnel := &v1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tunnel)).To(Succeed())
			customDeadline := int64(300)
			tunnel.Spec.ActiveDeadlineSeconds = &customDeadline
			Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, pod)).To(Succeed())
			Expect(pod.Spec.ActiveDeadlineSeconds).NotTo(BeNil())
			Expect(*pod.Spec.ActiveDeadlineSeconds).To(Equal(int64(300)))
		})

		It("should clean up all gateway resources on tunnel deletion", func() {
			controllerReconciler := newReconciler()

			By("running two reconciles to create all gateway resources")
			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, pod)).To(Succeed())

			By("deleting the tunnel CR")
			tunnel := &v1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tunnel)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tunnel)).To(Succeed())

			By("reconciling to execute finalizer cleanup")
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, &corev1.Pod{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, &networkingv1.NetworkPolicy{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &v1alpha1.Tunnel{}))).To(BeTrue())
		})

		It("should restrict egress to the correct target IP and allow UDP for WireGuard", func() {
			controllerReconciler := newReconciler()

			_, err := controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			networkPolicy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}, networkPolicy)).To(Succeed())

			Expect(networkPolicy.Spec.Egress).To(HaveLen(2))

			By("verifying the TCP egress rule targets the correct IP and port")
			tcpRule := networkPolicy.Spec.Egress[0]
			Expect(tcpRule.To).To(HaveLen(1))
			Expect(tcpRule.To[0].IPBlock).NotTo(BeNil())
			Expect(tcpRule.To[0].IPBlock.CIDR).To(Equal("10.0.0.10/32"))
			Expect(tcpRule.Ports).To(HaveLen(1))
			Expect(tcpRule.Ports[0].Protocol).NotTo(BeNil())
			Expect(*tcpRule.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(tcpRule.Ports[0].Port).NotTo(BeNil())
			Expect(tcpRule.Ports[0].Port.IntValue()).To(Equal(6379))

			By("verifying the UDP egress rule allows any destination")
			udpRule := networkPolicy.Spec.Egress[1]
			Expect(udpRule.To).To(BeEmpty())
			Expect(udpRule.Ports).To(HaveLen(1))
			Expect(udpRule.Ports[0].Protocol).NotTo(BeNil())
			Expect(*udpRule.Ports[0].Protocol).To(Equal(corev1.ProtocolUDP))
		})

		It("should set phase to Ready when pod is ready and gateway status is available", func() {
			reconciler := newReconciler()
			reconciler.FetchGatewayStatus = func(podIP string) (string, error) {
				return "mock-gateway-public-key", nil
			}

			By("running two reconciles to create all resources")
			_, err := reconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			By("simulating the pod becoming ready")
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Name: gatewayResourceName(resourceName), Namespace: namespace}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			pod.Status.Phase = corev1.PodRunning
			pod.Status.PodIP = "10.244.0.5"
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			By("reconciling again — operator should fetch public key and set Ready")
			_, err = reconciler.Reconcile(ctx, newRequest())
			Expect(err).NotTo(HaveOccurred())

			tunnel := &v1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tunnel)).To(Succeed())
			Expect(tunnel.Status.Phase).To(Equal(v1alpha1.TunnelPhaseReady))
			Expect(tunnel.Status.GatewayPublicKey).To(Equal("mock-gateway-public-key"))
			Expect(tunnel.Status.Message).To(Equal("Gateway ready"))
		})
	})
})
