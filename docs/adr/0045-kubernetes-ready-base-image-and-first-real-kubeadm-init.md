# ADR-0045: a real Kubernetes-ready base image, and the first genuine kubeadm init

## Status

Accepted

## Context

ADR-0031/ADR-0044 and the `cluster-api-provider-apiary` repo's own
kubeadm-bootstrap work got a real `Cluster`/`ApiaryCluster`/`Machine`/
`ApiaryMachine`/`KubeadmConfig` set all the way to a genuine, non-bypassed
kubeadm bootstrap secret, deployed against a real VM with a precomputed
deterministic MAC and static IP. That VM's own console showed cloud-init
genuinely attempting `kubeadm init` - proving the rendered bootstrap data
was real - but it failed immediately with `kubeadm: not found`, since
`ubuntu-cloud.raw` (ADR-0031's stock base image) has no Kubernetes
tooling installed at all.

The agreed next step: build a real "Kubernetes-ready" Linux disk image
(`containerd`/`kubeadm`/`kubelet`/`kubectl` pre-installed) and reference
it via the existing `base_image_name` mechanism (ADR-0031), instead of
`ubuntu-cloud.raw`. FreeBSD has no viable path here at all - confirmed
directly: no production CRI-compatible container runtime exists for
jails, and `kubeadm` itself has zero FreeBSD support, so even a
kubeadm-managed control-plane node's own static pods (etcd/apiserver/
scheduler) run through that same node's own `kubelet` - there is no way
to keep even the control plane on FreeBSD while workers are Linux.

## Approach

A temporary, from-scratch NoCloud cloud-init workflow (not committed -
see "Not part of this codebase" below) drives a throwaway VM through a
full package-install script, then captures its resulting disk file as a
new named base image via the existing `internal/isostore`/`UploadISO`
machinery (ADR-0017) - exactly the same mechanism a human uploads any
other base image or ISO through, just automated end to end. The disk
image itself is treated as build output, not source: no Apiary code
changed to produce it.

## Three real bugs found and fixed, each only visible via live boot testing

1. **Missing `conntrack`.** `apt-get install containerd` alone doesn't
   pull in `conntrack-tools` on Ubuntu 24.04's cloud image, and
   `kubeadm init`'s preflight check treats a missing `conntrack` binary
   as fatal (`[ERROR FileExisting-conntrack]: conntrack not found in
   system path`) - the same "not found" class of failure as the missing
   `kubeadm` binary itself, just one layer deeper. Fixed by explicitly
   installing `conntrack socat ebtables ethtool` alongside `containerd`
   (the last three are kubeadm's other commonly-recommended preflight
   dependencies, added preemptively rather than found one at a time).

2. **DHCP client-identifier drift, self-inflicted by the image build's
   own template-reuse step.** The build script resets `/etc/machine-id`
   at the end (`rm -f /etc/machine-id; touch /etc/machine-id`) so the
   captured image is safe to reuse as a template - matching cloud-init's
   own standard advice for cloned/templated images. But Ubuntu's default
   DHCPv4 client identifier (RFC 4361, a DUID) is *derived from*
   `/etc/machine-id`, not the interface's MAC - so a fresh boot from the
   captured image presents a *different* client identifier to the DHCP
   server than the stock image did, even with the exact same MAC. Since
   ADR-0044's whole point was a MAC-keyed static router reservation
   (`10.50.0.50` for `4e:50:cb:24:ab:af`), this silently broke it: the
   guest got a plain dynamic lease (`10.50.0.129`, then `10.50.0.133` on
   a second attempt) instead of the reserved address.

   The first fix attempt - a netplan-level override
   (`/etc/netplan/99-dhcp-identifier.yaml`, a separate device id merged
   by netplan) - did **not** work, and this itself is worth recording:
   systemd-networkd applies only the *first* matching `.network` file
   per interface, by filename sort order across `/etc` and `/run` - it
   does not merge multiple matching files by device id the way netplan's
   own YAML merge model implies. cloud-init's own generated netplan
   config renders to `/run/systemd/network/10-netplan-*.network` and won
   outright over our separately-named device entry, which simply never
   applied.

   Fixed by bypassing netplan for this setting entirely: a low-numbered
   drop-in written directly to `/etc/systemd/network/05-dhcp-mac.network`
   (`[Match] Name=en*`, `[Network] DHCP=yes`, `[DHCPv4]
   ClientIdentifier=mac`) sorts before cloud-init's `10-netplan-*.network`
   files and so is the one systemd-networkd actually picks, replacing
   cloud-init's own DHCP config for that interface outright (not merging
   with it - our file has to be complete on its own, which it is).

3. **(Observed only, not a code bug)** kubeadm init's own container-image
   pull and static-pod bring-up needs real internet reachability from the
   guest - unremarkable on its own, but worth noting since it's why each
   of the three test cycles above took real wall-clock minutes rather
   than seconds; not something a build script can shortcut.

## Verification - real, live, end to end

After all three rebuild cycles, a fresh boot of the final image:

- Received the DHCP address `10.50.0.50` - the fix confirmed working
  (a boot from the still-broken second attempt had gotten `10.50.0.133`
  moments earlier, same VM id, same MAC, same router reservation).
- Ran `kubeadm init` past preflight (no conntrack error), through image
  pulls, kubelet start, and the control-plane static-pod health check,
  to a genuine **`Your Kubernetes control-plane has initialized
  successfully!`** banner - the first time this has happened anywhere
  in this project's history, non-bypassed, via the real upstream kubeadm
  bootstrap provider.
- Independently confirmed from outside the guest entirely:
  `curl -k https://10.50.0.50:6443/livez` returned `ok`, and `/version`
  reported a genuine `v1.31.0` apiserver - not just console output
  claiming success, an actually externally-reachable control plane.

The VM (`capi-f21194fcd6cb` / `apiary-cp-1`) was left running as a real,
working control-plane node.

## Not part of this codebase

The image-build script and its supporting one-off Go tooling (a NoCloud
seed builder invocation, a streaming multipart uploader for the ~40 GB
disk file, small wrappers around the existing `apiaryclient`/`UploadISO`
REST calls) were written and run as temporary, uncommitted tooling on
`apiarium`/`apiverse` directly - not added to this repository or to
`cluster-api-provider-apiary`, and deleted after use. The durable
artifact of this work is the uploaded base image itself
(`ubuntu-k8s-1.31.raw`, SHA-256
`09f28031f2c025fcbd147b948510ff151fa3b2dd5069925671248f0dfed34b19`,
stored in `apiverse`'s isostore) plus this ADR's record of what it
contains and why. A reproducible, checked-in build script (e.g. Packer-
style) is real, disclosed future work if this image needs to be rebuilt
or audited later - not attempted this pass.

## Also confirmed while streaming the ~40 GB upload

`internal/isostore`'s existing `UploadISO`/`Save` path was never
previously exercised with a file this large from a from-scratch client.
The `cluster-api-provider-apiary` repo's own `apiaryclient.UploadISO`
buffers its entire multipart body into a `bytes.Buffer` before sending -
fine for a few-KB seed ISO, but would have meant holding the whole ~40 GB
disk image in memory at once, a real risk on `apiverse`'s 32 GB of RAM.
Worked around with a small, uncommitted streaming variant
(`io.Pipe`-based, bounded memory) for this one-off transfer rather than
changing `apiaryclient` itself - not treated as an Apiary-side bug, since
`internal/isostore.Save` on the receiving end already streams correctly
(`io.Copy` into a temp file, no full-body buffering); the buffering issue
was entirely client-side, in the separate CAPI repo's convenience helper.
