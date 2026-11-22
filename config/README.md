# netbox-provider deployment manifests

The layout follows Fleet's `config/` conventions. The controller reads NetBox,
keeps its own configuration kinds on the cluster it runs in, and creates Fleet
inventory records in a Milo provider project. See the
[design](../docs/architecture/design.md) for the import rules.

The repository publishes one production manifest root.

| Root | Destination | Contents |
| --- | --- | --- |
| `config/overlays/management-plane` | The cluster that runs the controller | `netbox.miloapis.com` CRDs, the `netbox-provider-system` namespace, the manager workload, and its namespaced RBAC |
| `config/standalone` | Disposable local cluster (non-production only) | The management-plane resources plus a same-cluster stand-in for the provider project, a synthetic token, RBAC and fixtures |

`config/base/crd` renders the CRDs alone. `test/fleet/crds` is Fleet's vendored
inventory schema for tests and the standalone cluster; it is not a production
root, and Fleet stays its owner.

There is no `config/milo` root. This controller's kinds are configuration for
one deployment, not project resources, so they are not registered in Milo and
have no ProtectedResources. The Milo IAM the controller needs is Fleet's
[importer role](../docs/architecture/fleet-contract.md#the-importer-role), extended with `patch` for server-side apply,
which ships in Fleet's own `config/milo`.

`config/standalone` must not be referenced by the production root. It points a
NetBoxSource at a reserved `.invalid` name, so it shows the fail-closed path:
`SourceObserved` and `Ready` stay Unknown and nothing is written. Deploy it with
`task standalone:setup`, which installs the vendored Fleet CRDs first.

Render every root before publishing the Kustomize bundle:

```sh
task manifests:validate
```

## Published artifacts

`.github/workflows/publish.yml` publishes the multi-architecture manager image
as `ghcr.io/datum-labs/netbox-provider` and then publishes `config` as
`ghcr.io/datum-labs/netbox-provider-kustomize`. The bundle publisher stamps the
exact manager image tag into `config/base/manager`, so an environment consumes
one matched image and manifest revision. Consumers select
`config/overlays/management-plane` from the bundle.

The image can be built locally with:

```sh
docker build -f build/manager.Dockerfile .
```

Deployment owners must select an immutable workflow-produced tag or digest and
replace `ghcr.io/datum-labs/netbox-provider:latest`.

## Environment-owned inputs

The management-plane root intentionally does not contain a
`netbox-provider-manager-config` ConfigMap. The environment overlay must create
it in `netbox-provider-system` with a `config.yaml` key. The mount paths below
match the Deployment:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: netbox-provider-manager-config
  namespace: netbox-provider-system
data:
  config.yaml: |
    apiVersion: apiserver.config.miloapis.com/v1alpha1
    kind: NetBoxSourceOperator
    metricsServer:
      bindAddress: "0"
    fleet:
      kubeconfigPath: /var/run/netbox-provider/fleet/kubeconfig
```

`kubeconfigPath` (top level) stays empty: the controller's own kinds and token
Secrets are on the in-cluster API server. A missing ConfigMap prevents the Pod from starting; an invalid configuration makes the binary exit.

The environment must provide in `netbox-provider-system`:

- `netbox-provider-fleet-kubeconfig`, key `value`: a Milo kubeconfig whose host
  is the provider project's control plane,
  `/apis/resourcemanager.miloapis.com/v1alpha1/projects/<provider project>/control-plane`.
  Fleet inventory lives there. A missing Secret prevents the Pod from starting.
- One Secret per NetBoxSource holding a read-only NetBox API token, named by
  its `spec.tokenSecretRef`.
- The NetBoxSources and NetBoxMappings themselves. They are environment state,
  not bundle content.

Kubeconfigs, tokens, mappings and rendered Secrets remain owned by the
environment and must not be added to this repository.

### Role bindings

Bind the controller's Milo identity, in its one provider project, to a role
with `get` and `list` on the physical kinds and `create` and `patch` on Rack,
PowerFeed, DeviceClaim, Device and Cable. Server-side apply needs both: `create`
for a record's first apply and `patch` for later ones. Fleet's
`fleet.datumapis.com-inventory-importer` role (datum-cloud/fleet#101) grants
`create` only, so it is not enough on its own; see
[Fleet usage](../docs/architecture/fleet-contract.md#the-importer-role). No
`update` or `delete` is needed. Binding the role is the environment's
ownership opt-in; each NetBoxSource's `mode: Create` is the second, per-source
opt-in.

The management-cluster service account has no cluster-wide role. It receives
leader-election access and read access to NetBoxSources, NetBoxMappings and
Secrets, all in `netbox-provider-system`, and status writes on NetBoxSources.
The manager's cache is scoped to that namespace with `--namespace`, so
NetBoxSources in other namespaces are ignored. Secrets are fetched with `get`
only and never cached. The namespace has Flux pruning disabled so uninstalling
a release cannot delete environment-owned sources, mappings or tokens.

The manager holds one lease (`netbox-provider-manager.netbox.miloapis.com`) in
`netbox-provider-system`; only the holder reconciles.

## Readiness boundary

`/readyz` and `/healthz` on port `health` (8081) report process health only.
They do not depend on NetBox or Fleet. Import state is in each NetBoxSource's
conditions: failed or partial reads are Unknown, and `Ready` describes that
import, never the readiness of a Fleet record or of hardware.

## Differences from Fleet

| Fleet convention | Here | Why |
| --- | --- | --- |
| `config/milo` root with CRDs, ProtectedResources and roles | None | The kinds stay on the local cluster; the IAM needed is Fleet's permission set and is proposed to Fleet. |
| `fleet-system-private` namespace and private-record role | None | No private records. Token Secrets live in `netbox-provider-system` with get-only access. |
| Two Milo kubeconfigs (discovery and project) | One, `netbox-provider-fleet-kubeconfig` | The controller writes one known provider project; it discovers nothing. |
| Root kubeconfig Secret `milo-configuration-kubeconfig` | Not used | The controller never needs root Milo access. |
| `--private-namespace` scopes the private-record cache | `--namespace=netbox-provider-system` scopes the whole cache | Without it, reading NetBoxSources would need a cluster-wide role. |
| Container has no explicit `command` | `command: [/netbox-provider]` plus `operator` subcommand | The template binary has subcommands. |
| Admission component, UI plugin components, simulator, synthetic-staging overlay | None | Nothing equivalent exists for this controller. |
| CI job runs steps directly | Steps run inside the template's `ci-basic` workflow | `ci / Build and test` is the required check for this repository. Kustomize is run through `go run` at Fleet's pinned 5.8.1. |
| `datum-cloud/actions` v1.21.0 | v1.28.0 | The template's pin; the `images` input used by Fleet exists in both. |
| No lint or kustomize-sweep workflows | Kept `lint.yml` and `validate-kustomize.yaml` | Template checks that run offline and cost nothing. |
