# netbox-provider

A Kubernetes controller that imports NetBox physical inventory into Fleet,
generated from `milo-os/controller-template` (kubebuilder v4). Read
[AGENTS.md](AGENTS.md) first: its safety rules override anything below.

## Architecture

- **CRD-based**: kubebuilder v4 CRDs in `netbox.miloapis.com/v1alpha1`
  (`NetBoxSource`, `NetBoxMapping`), validated with CEL. No webhooks.
- **Deployment**: `config/` follows Fleet's layout and RBAC split. RBAC is
  hand-written and namespaced; do not add kubebuilder RBAC markers.
- **Two API servers**: the manager's own (`kubeconfigPath`) stores this
  controller's kinds and token Secrets; Fleet records are read, and written by
  server-side apply as field manager `netbox-provider`, on
  `fleet.kubeconfigPath`, normally the Milo provider project control plane.
- **Fleet as unstructured**: `internal/fleet` mirrors only the written fields;
  `test/fleet/crds` vendors Fleet's CRD for envtest.

## Repo Layout

```
netbox-provider/
├── cmd/netbox-provider/        # Binary entrypoint and cobra commands
├── api/v1alpha1/               # NetBoxSource and NetBoxMapping types
├── internal/
│   ├── config/                 # Operator configuration (NetBoxSourceOperator)
│   ├── controller/             # Reconciler and envtest suite
│   ├── fleet/                  # Source-agnostic Fleet mirror and naming rule
│   ├── netbox/                 # Bounded GET-only NetBox reader (+ netboxtest fake)
│   └── plan/                   # Pure planning: NetBox + mapping + Fleet -> plan
├── build/manager.Dockerfile    # Manager image
├── config/                     # Kustomize roots in Fleet's layout; see config/README.md
├── docs/architecture/          # Design and how the controller uses Fleet
├── test/fixtures/netbox/       # Synthetic NetBox collections and mapping
├── test/fleet/                 # Vendored Fleet CRD and where it came from
└── hack/                       # Boilerplate and doc checks
```

## Connecting to the Milo Control Plane

```yaml
apiVersion: apiserver.config.miloapis.com/v1alpha1
kind: NetBoxSourceOperator
metricsServer:
  bindAddress: "0"
fleet:
  kubeconfigPath: /etc/milo/kubeconfig
```

When a kubeconfig path is empty, the controller falls back to in-cluster
config, the default for local kind development.

## Reconcile errors

Return an error from `Reconcile` only when a retry can fix it. controller-runtime retries every error with backoff and counts it, so an error that no retry can fix repeats for ever as a steady error rate.

- **A failure no retry can fix**, such as input that can never succeed or an object a person must act on: set a condition that names the case and return `reconcile.TerminalError(err)`. It is logged and counted once, and not retried.
- **A wait for something else**, such as another object becoming ready or dependents going away: set a condition, watch what you wait for, and return no error. When you cannot watch it, return `ctrl.Result{RequeueAfter: <poll interval>}`. Do not use `Result.Requeue`, which controller-runtime deprecates. [resend-provider's `finalizer_wait.go`](https://github.com/milo-os/resend-provider/blob/6c8e2b0745735dfffe1dd469278ae19ab03abc32/internal/controller/finalizer_wait.go) does this for a finalizer.
- **Anything else**, such as a timeout, a conflict or a server error: return the error, and controller-runtime retries it.

In this controller: NetBox and Fleet read failures set Unknown conditions and
return the error; a missing Secret or mapping sets a condition and polls or
watches; an invalid URL that passed admission is terminal.

## Verification Commands

```bash
task build                    # Build binary
task test                     # Run tests with envtest
task lint                     # Run linter
task validate                 # gofmt, vet, kustomize, doc links
task generate                 # Run code generation
task manifests                # Generate config/base/crd/netbox.miloapis.com.yaml
task manifests:validate       # Render every kustomize entry point
task standalone:setup         # Bootstrap kind cluster + synthetic standalone root
task standalone:redeploy      # Rebuild and restart the standalone manager
```

## Metrics and logs

Create every metric and logger through go.miloapis.com/instrumentation (Go),
naming labels with its labels constants. The instrumentation adoption test
enforces this. When it fails, follow its message, which links to
https://github.com/milo-os/instrumentation/blob/main/ADOPTING.md.
