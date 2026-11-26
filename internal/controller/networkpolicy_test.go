package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func npPort(proto corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: new(proto), Port: new(intstr.FromInt32(port))}
}

var (
	wantDNS = networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolUDP, 53), npPort(corev1.ProtocolTCP, 53)}}
	wantNet = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{
		CIDR:   "0.0.0.0/0",
		Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"},
	}}
	kiwixPods = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "kiwix", "app.kubernetes.io/instance": "wiki"}}
	crawlPods = &metav1.LabelSelector{MatchLabels: map[string]string{"kiwix.jacaudi.dev/library": "wiki"}}
)

func cidrPeers(cidrs ...string) []networkingv1.NetworkPolicyPeer {
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, c := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
	}
	return peers
}

func TestNetworkPolicies(t *testing.T) {
	librarianIngress := networkingv1.NetworkPolicyIngressRule{
		Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, 8090)},
		From: []networkingv1.NetworkPolicyPeer{
			{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kiwix-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "kiwix-operator"}},
			},
			{PodSelector: crawlPods},
		},
	}
	crawlToLibrarian := networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, 8090)},
		To:    []networkingv1.NetworkPolicyPeer{{PodSelector: kiwixPods}},
	}
	ingressFrom := []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "network"}}}}

	tests := []struct {
		name         string
		ingressFrom  []networkingv1.NetworkPolicyPeer
		egressAllow  []string
		probeAllow   []string
		kiwixIngress []networkingv1.NetworkPolicyIngressRule
		internet     networkingv1.NetworkPolicyEgressRule
	}{
		{
			name: "defaults",
			kiwixIngress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, 8080)}},
				librarianIngress,
			},
			internet: networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{wantNet}},
		},
		{
			name:        "ingressFrom, egress and probe CIDRs",
			ingressFrom: ingressFrom,
			egressAllow: []string{"10.244.0.0/16", "10.96.0.0/12"},
			probeAllow:  []string{"172.18.0.0/16"},
			kiwixIngress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, 8080)}, From: ingressFrom},
				librarianIngress,
				{Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, 8080), npPort(corev1.ProtocolTCP, 8090)}, From: cidrPeers("172.18.0.0/16")},
			},
			internet: networkingv1.NetworkPolicyEgressRule{To: append([]networkingv1.NetworkPolicyPeer{wantNet}, cidrPeers("10.244.0.0/16", "10.96.0.0/12")...)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := testLib()
			l.Spec.Server.IngressFrom = tt.ingressFrom

			got := buildNetworkPolicies(l, "kiwix-system", tt.egressAllow, tt.probeAllow)

			require.Len(t, got, 2)
			kiwix, crawl := got[0], got[1]

			assert.Equal(t, "wiki-kiwix", kiwix.Name)
			assert.Equal(t, "kiwix", kiwix.Namespace)
			assert.Equal(t, "kiwix-operator", kiwix.Labels["app.kubernetes.io/managed-by"])
			assert.Equal(t, *kiwixPods, kiwix.Spec.PodSelector)
			assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, kiwix.Spec.PolicyTypes)
			assert.Equal(t, tt.kiwixIngress, kiwix.Spec.Ingress)
			assert.Equal(t, []networkingv1.NetworkPolicyEgressRule{wantDNS, tt.internet}, kiwix.Spec.Egress)

			assert.Equal(t, "wiki-crawl", crawl.Name)
			assert.Equal(t, "kiwix", crawl.Namespace)
			assert.Equal(t, "kiwix-operator", crawl.Labels["app.kubernetes.io/managed-by"])
			assert.Equal(t, *crawlPods, crawl.Spec.PodSelector)
			assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, crawl.Spec.PolicyTypes)
			assert.Empty(t, crawl.Spec.Ingress)
			assert.Equal(t, []networkingv1.NetworkPolicyEgressRule{wantDNS, crawlToLibrarian, tt.internet}, crawl.Spec.Egress)
		})
	}
}
