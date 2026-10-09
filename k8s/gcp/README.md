# kind to Google inference identity

This optional OpenTofu module configures one Kubernetes ServiceAccount to
impersonate a dedicated Google service account for Vertex inference. It creates
or manages a workload identity pool/provider, enables the required APIs, and
grants only the inference service account the Vertex User and Service Usage
Consumer roles. The provider condition requires the configured namespace and
exact ServiceAccount subject.

The module reads the **public** kind JWKS from `jwks_file`. Keep the JWKS and
variable values in ignored local files. Do not put service-account keys,
project identifiers, or generated state in tracked files. OpenTofu and the
Google provider versions are pinned in `TOFU_VERSION` and `main.tf`.

## Configure

1. Start the owned kind cluster and write its current public keys:

   ```sh
   k8s/kind/01-kind.sh --profile fixture
   kubectl --kubeconfig k8s/.local/kubeconfig --context kind-fullsend-poc \
     get --raw /openid/v1/openid-configuration | jq -er .issuer
   kubectl --kubeconfig k8s/.local/kubeconfig --context kind-fullsend-poc \
     get --raw /openid/v1/jwks > k8s/.local/gcp/kind-jwks.json
   ```

   Check that the JWKS contains public keys only. The kind issuer's signing
   keys change when the cluster is recreated.

2. Create an ignored variable file from the example, filling in the development
   project values and the issuer printed above:

   ```sh
   cp k8s/gcp/terraform.tfvars.example k8s/gcp/local.auto.tfvars
   ```

3. Initialize and review the plan:

   ```sh
   tofu -chdir=k8s/gcp init
   tofu -chdir=k8s/gcp plan -var-file=local.auto.tfvars
   ```

   The existing PoC may already have a pool, provider, service account, API
   enablement, and IAM bindings. Import each existing resource into local state
   before applying; do not try to create duplicate resources. For example:

   ```sh
   tofu -chdir=k8s/gcp import -var-file=local.auto.tfvars \
     google_iam_workload_identity_pool.kind \
     projects/PROJECT_ID/locations/global/workloadIdentityPools/POOL_ID
   tofu -chdir=k8s/gcp import -var-file=local.auto.tfvars \
     google_iam_workload_identity_pool_provider.kind \
     projects/PROJECT_ID/locations/global/workloadIdentityPools/POOL_ID/providers/PROVIDER_ID
   tofu -chdir=k8s/gcp import -var-file=local.auto.tfvars \
     google_service_account.inference \
     projects/PROJECT_ID/serviceAccounts/ACCOUNT_ID@PROJECT_ID.iam.gserviceaccount.com
   ```

   Import any existing project API and IAM member resources shown by the plan
   as well. Keep the generated state local and ignored.

4. Apply only after the plan shows the intended resources and no unintended
   replacements. After each kind recreation, export the new JWKS to the path in
   `local.auto.tfvars`, then run `tofu apply` to update the provider's trusted
   keys. Do not rely on a stable issuer string to imply stable signing keys.

Outputs provide the provider audience and inference service account email for
the runner probes. `k8s/kind/15-wif-inference.sh` tests authorized inference
before and after projected-token rotation, and verifies that a different
ServiceAccount is rejected. `k8s/kind/16-wif-fullsend.sh` exercises the normal
Fullsend run path through HyperShell. Its loopback-only refresh endpoint
delivers only the inference-audience assertion, and the remote sandbox makes a
Vertex request after that assertion rotates. The integration evidence records
assertion hashes and outcomes, not assertions or access tokens.

The module is opt-in. The default fixture reset does not initialize or apply
cloud resources.
