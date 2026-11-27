# Security

This is an explanation of the operator's security model: what each pod may
reach, how the pods are locked down, how the librarian is authenticated, and
what risk is accepted.

## NetworkPolicies

Each `KiwixLibrary` gets two NetworkPolicies, enforced only on a CNI that
supports them. The policies assume an IPv4-only cluster.

- **Kiwix pod ingress.** The kiwix-serve port (8080) admits
  `spec.server.ingressFrom` (empty means any). The librarian port (8090)
  admits only the operator's pods and this library's crawl pods, selected by
  the label `kiwix.jacaudi.dev/library: <name>`. The operator is matched by
  namespace and by the pod label `app.kubernetes.io/name: kiwix-operator`,
  which is why `global.nameOverride` must stay unset.
- **Kiwix pod egress.** DNS (UDP and TCP 53) and `0.0.0.0/0` except the private
  ranges below, because the librarian fetches user-supplied URLs and follows
  redirects.
- **Crawl pod egress.** DNS, the librarian port on this library's pod, and
  `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `100.64.0.0/10` and `169.254.0.0/16`. A hostile page cannot reach in-cluster
  services.

Two chart values widen the policies. `crawlEgressAllowCIDRs` adds CIDRs to both
egress allow-lists; the kind e2e uses it because its test site is in-cluster.
`probeAllowCIDRs` lets the given CIDRs reach both kiwix pod ports, for CNIs that
filter kubelet probes through the policy.

## Pod Hardening

- **Kiwix pod.** Runs as uid and gid 1000 with `fsGroup: 1000`, drops all
  capabilities, forbids privilege escalation, uses the `RuntimeDefault`
  seccomp profile and a read-only root filesystem with an `emptyDir` at
  `/tmp`, and does not mount a service account token. `/data` is read-only in
  kiwix-serve and read-write only in the librarian.
- **Crawl pod.** zimit may run as root, but it drops all capabilities, forbids
  privilege escalation and uses `RuntimeDefault` seccomp; it is never
  privileged and no capability is added back. The upload container runs as uid
  1000 with the same restrictions, and the pod's `fsGroup` lets it read what
  zimit wrote.
- **Crawl pod service account.** No token is mounted.

## Authentication

- The manager reaches the librarian with a per-library admin token in the Secret
  `<library>-librarian-admin`.
- Each crawl attempt gets its own upload token. The manager keeps it in a Secret
  mounted only into the upload container, never into zimit, and the librarian
  stores only its hash.
- Path segments are validated before any filesystem use.

## Accepted Risks

- A compromised Chromium could make zimit write a crafted ZIM that the upload
  container sends. The health gate runs after libzim parses the file, so it
  cannot stop a parser exploit, and the librarian has read-write `/data`.
  Isolating the parse is a planned hardening.
- The webhook URL is whatever the `KiwixLibrary` author sets. The manager does
  not filter it, so in-cluster targets work.

[← Back to README](../README.md)
