# netbox-provider

A Kubernetes controller that imports physical inventory from NetBox into Fleet.
It reads NetBox, compares it with the Fleet inventory records that already
exist, and reports a plan. When you opt in, it creates the records that are
missing. It never changes, adopts or deletes an existing record.

Fleet does not know this controller or NetBox exists. Fleet sees ordinary
inventory records holding only physical facts; this controller correlates them
with NetBox through its own `netbox.miloapis.com/*` labels and annotations. It
targets Fleet's inventory schema without authorship fields (datum-cloud/fleet#105
and #106) and needs the create-only importer role (#101).

Generated from [`milo-os/controller-template`](https://github.com/milo-os/controller-template)
(kubebuilder v4). Tested against a synthetic NetBox and Fleet's vendored CRD
only; it has not run against a real NetBox or Milo control plane.

## How it works

You create two resources in the controller's own cluster:

- a **NetBoxSource**: the NetBox URL, a Secret holding a read-only API token,
  a stable source ID, an optional site scope, and the Fleet namespace to write
  to; and
- a **NetBoxMapping**: your reviewed decisions. It maps NetBox sites and
  locations to existing Fleet Facilities, and says for each device whether it
  is physical, what its serial field means, and how precisely to place it.

Each reconcile reads NetBox and the Fleet namespace completely, then plans.
Each Fleet record in the plan gets one outcome:

| Outcome | Meaning |
| --- | --- |
| Create | Missing from Fleet and safe to create. |
| Unchanged | Already in Fleet from this source with the same facts. |
| Quarantined | Conflicts with an existing record, such as a name owned by another writer or a matching serial. Nothing is written. |
| Unresolved | Needs a mapping decision, or depends on something blocked. |

In the default `Observe` mode nothing is written. Set `spec.mode: Create` to
create the planned records: racks first, then device claims, then devices. The
status lists conflicts first and never contains serials or tokens. Any failed
or uncertain read or write is reported as `Unknown`, never `Ready`.

It imports racks, device claims and devices. Facilities stay operator-owned.
Cables, power, components, circuits and Sites are not imported yet.

Read the [design](docs/architecture/design.md) for the full rules, and the
[Fleet usage](docs/architecture/fleet-contract.md) for exactly what it writes
and the one Fleet dependency, the create-only importer role.

## Deploy

The deployment layout follows Fleet's. Consumers apply
`config/overlays/management-plane` from the published
`ghcr.io/datum-labs/netbox-provider-kustomize` bundle and supply the
environment-owned ConfigMap, Milo kubeconfig, token Secrets, NetBoxSources and
NetBoxMappings described in [config/README.md](config/README.md). Fleet records
are written through Milo IAM in the provider project, never local RBAC.

Records carry only physical facts; the link to NetBox is in this controller's
labels and annotations. A Device is created only when its NetBox serial is a
reviewed chassis serial; otherwise its DeviceClaim is created and the Device is
reported Unresolved.

Review a source's plan in Observe mode first; use a dedicated NetBox account
with a read-only token. When the plan is what you expect, opt in:

```sh
kubectl get netboxsources -n netbox-provider-system -o yaml   # status.plan, status.records
kubectl patch netboxsource example -n netbox-provider-system --type merge -p '{"spec":{"mode":"Create"}}'
```

To try it locally, `task standalone:setup` deploys the synthetic
[standalone root](config/standalone) into a disposable kind cluster.

## Development

You need Go (the toolchain in `go.mod` is fetched automatically), Task,
[`setup-envtest`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/tools/setup-envtest),
kustomize, and Bash. Docker and kind are only needed for `task standalone:setup`.

```sh
task generate     # deepcopy code (controller-gen)
task manifests    # config/base/crd/netbox.miloapis.com.yaml from kubebuilder markers
task build        # bin/netbox-provider
task test         # go test ./... with envtest assets for Kubernetes 1.31.0
task lint         # golangci-lint
task validate     # gofmt, go vet, manifests:validate, doc links and whitespace
task manifests:validate  # render every kustomize entry point
task standalone:setup    # kind cluster, vendored Fleet CRDs, synthetic standalone root
```

`task test` runs:

```sh
KUBEBUILDER_ASSETS="$(setup-envtest use 1.31.0 -p path)" go test -timeout 10m ./... -count=1
```

Without Task:

```sh
go build ./...
go vet ./...
test -z "$(gofmt -l ./api ./cmd ./internal ./test)"
KUBEBUILDER_ASSETS="$(setup-envtest use 1.31.0 -p path)" go test ./...
go test -race ./...   # also needs KUBEBUILDER_ASSETS
bin/golangci-lint run ./...
kustomize build config/base/crd >/dev/null
kustomize build config/overlays/management-plane >/dev/null
kustomize build config/standalone >/dev/null
kustomize build test/fleet/crds >/dev/null
bash hack/validate-doc-links.sh
```

The first `task` run asks you to trust the remote test-infra Taskfile; pass
`--yes` to accept. `task generate` and `task manifests` install pinned tools
into `bin/`.

| Tests | What they prove |
| --- | --- |
| `internal/netbox` | GET-only reads, path prefixes, auth schemes, pagination safety, scope-filter preservation, count and ID consistency, byte, page and record limits, redirects, cancellation, no partial results, no response bodies in errors, field allowlisting, stable digests |
| `internal/plan` | Facility mapping, location precedence, serial meanings, placement scopes, stable names, unchanged records, quarantines for name conflicts, differing records, duplicate sources and identity collisions, dependency blocking, retention of omitted sources |
| `internal/controller` (envtest) | Generated records pass the vendored Fleet CRD admission; this controller's CRD validation; observe-only default; opt-in create then no-op; existing-name rejection; identity quarantine; no deletion on omission; NetBox and Fleet failures as Unknown; token Secret handling; manager wiring |
| `cmd/.../cmd`, `internal/config` | The template's instrumentation adoption test; operator config defaults |

The envtest suite installs this repository's CRDs and the
[vendored Fleet inventory CRD](test/fleet/README.md). All fixtures are
synthetic; no test contacts a real NetBox or cluster.

## Repository rules

See [AGENTS.md](AGENTS.md) and [CLAUDE.md](CLAUDE.md).
