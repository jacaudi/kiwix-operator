package controller

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

// privateRanges are excluded from the internet egress rule, so in-cluster and link-local targets need an explicit peer.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}

func tcpPorts(ports ...int32) []networkingv1.NetworkPolicyPort {
	out := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, networkingv1.NetworkPolicyPort{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(p))})
	}
	return out
}

func ipPeers(cidrs []string) []networkingv1.NetworkPolicyPeer {
	out := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
	}
	return out
}

// buildNetworkPolicies returns the kiwix pod's policy and the crawl pods' policy. egressAllow is added to both internet rules; probeAllow may reach both kiwix ports.
func buildNetworkPolicies(lib *v1alpha1.KiwixLibrary, operatorNS string, egressAllow, probeAllow []string) []*networkingv1.NetworkPolicy {
	dns := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{
		{Protocol: new(corev1.ProtocolUDP), Port: new(intstr.FromInt32(53))},
		{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(53))},
	}}
	internet := networkingv1.NetworkPolicyEgressRule{To: append(
		[]networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: slices.Clone(privateRanges)}}},
		ipPeers(egressAllow)...,
	)}
	kiwixPods := metav1.LabelSelector{MatchLabels: podSelector(lib)}
	crawlPods := metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelLibrary: lib.Name}}

	ingress := []networkingv1.NetworkPolicyIngressRule{
		{Ports: tcpPorts(servePort), From: lib.Spec.Server.IngressFrom},
		{Ports: tcpPorts(librarianPort), From: []networkingv1.NetworkPolicyPeer{
			{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: operatorNS}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelName: operatorAppName}},
			},
			{PodSelector: &crawlPods},
		}},
	}
	if len(probeAllow) > 0 {
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{Ports: tcpPorts(servePort, librarianPort), From: ipPeers(probeAllow)})
	}

	kiwix := &networkingv1.NetworkPolicy{
		ObjectMeta: objectMeta(lib, lib.Name+"-kiwix", podLabels(lib)),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: kiwixPods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      []networkingv1.NetworkPolicyEgressRule{dns, internet},
		},
	}
	crawl := &networkingv1.NetworkPolicy{
		ObjectMeta: objectMeta(lib, lib.Name+"-crawl", podLabels(lib)),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: crawlPods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dns,
				{Ports: tcpPorts(librarianPort), To: []networkingv1.NetworkPolicyPeer{{PodSelector: &kiwixPods}}},
				internet,
			},
		},
	}
	return []*networkingv1.NetworkPolicy{kiwix, crawl}
}
