# IaC (Infrastructure as Code) - EKS Platform Foundation

This directory contains the **Terraform** configuration for standing up the production-grade base Kubernetes platform on AWS EKS, as specified in `docs/architecture/06-blueprint.md` and `docs/architecture/05-security.md`.

## Architectural Decision Log: Why Terraform?

1. **Standard & Automation**: Diagram 8 explicitly mandates: *"Infrastructure as Code is mandatory. All platform changes must be version-controlled and audit-logged. No manual cluster clicking."* Terraform is the most widely adopted tool for managed cloud-native infrastructures.
2. **Resource Dependency Orchestration**: Provisioning an EKS cluster requires orchestrating underlying AWS resources (VPC, IAM Roles, Subnets, Routing, OIDC providers, KMS keys, NAT Gateways). Terraform models this DAG (Directed Acyclic Graph) of resource dependencies cleanly.
3. **Workload Identity (IRSA)**: Setting up secure IAM Roles for Service Accounts requires an OIDC identity provider mapped to IAM policies. Terraform can easily provision the AWS OIDC provider and output the IAM ARNs needed by Kubernetes service accounts.
4. **Cloud Portability**: By using modular Terraform configurations, the exact same structure (networks, IAM roles, namespaces, network policies) can easily be ported to GCP GKE or Azure AKS by replacing AWS-specific resources with their cloud equivalents.

---

## VPC & Security Topology

- **Isolation**: Workloads are deployed entirely inside **Private Subnets**. Worker nodes do not have public IP addresses and cannot receive unsolicited inbound connection attempts from the internet.
- **Outbound Ingress**: Workers use highly resilient AWS NAT Gateways to access external APIs (e.g. banking connectors, tax authorities, Docker registries) while remaining protected.
- **Node Groups**: EC2 instances reside in private subnets, bound to standard node group security profiles managed by AWS EKS.

---

## Workload Identity (IRSA)

The Terraform setup deploys an **OpenID Connect (OIDC) Identity Provider** matching the EKS issuer URL. This allows Kubernetes Service Accounts to assume AWS IAM Roles securely without storing access keys in secrets (SOC 2 / zero-trust compliant).

To link a Kubernetes pod (e.g., `audit-svc` which needs access to an S3 bucket or KMS key) to an IAM Role, annotate the Service Account:
```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: audit-svc-account
  namespace: zoiko-evidence
  annotations:
    eks.amazonaws.com/role-arn: "arn:aws:iam::123456789012:role/eks-audit-svc-role"
```

---

## EKS Cluster Provisioning Instructions

### Prerequisites
1. Installed **AWS CLI** and configured IAM credentials (`aws configure`).
2. Installed **Terraform CLI** (v1.5.0+).
3. Installed **kubectl**.

### 1. Configure Remote State Backend (Collaborative Dev)
Before executing Terraform commands with multiple team members:
1. Create an S3 Bucket (e.g., `zoiko-terraform-state`) and a DynamoDB table for locking (e.g., `zoiko-terraform-locks` with Partition Key `LockID`).
2. Uncomment the `backend "s3"` block in `providers.tf` and supply your actual resource names.
3. Run `terraform init`.

### 2. Provisioning the Cluster
1. Initialize Terraform:
   ```bash
   terraform init
   ```
2. Check execution plan:
   ```bash
   terraform plan -out=tfplan
   ```
3. Provision cluster:
   ```bash
   terraform apply tfplan
   ```
4. Authenticate local `kubectl` to the new cluster:
   ```bash
   # Use the command from Terraform outputs
   aws eks update-kubeconfig --region us-east-1 --name zoiko-eks-cluster
   ```

---

## Bridging Kind Local Manifests to Production (EKS)

The base manifests under `../manifests/` are pre-configured with dev defaults suitable for local verification via `kind` (e.g. `:latest` tags, local database coordinates, and test-key secrets). 

To deploy these workloads onto a live **AWS EKS** cluster, apply the following steps:

### 1. Build, Tag & Push to AWS ECR
EKS cannot pull local Docker images or `:latest` tags from a local registry.
1. Authenticate Docker to AWS ECR:
   ```bash
   aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin <aws_account_id>.dkr.ecr.us-east-1.amazonaws.com
   ```
2. Create repositories for each microservice and push version-tagged images:
   ```bash
   # Example for identity-svc
   docker tag identity-svc:latest <aws_account_id>.dkr.ecr.us-east-1.amazonaws.com/identity-svc:v1.0.0
   docker push <aws_account_id>.dkr.ecr.us-east-1.amazonaws.com/identity-svc:v1.0.0
   ```
3. Update the deployment manifests to reference the ECR image URI and version tag (e.g. `<aws_account_id>.dkr.ecr.us-east-1.amazonaws.com/identity-svc:v1.0.0`) instead of `identity-svc:latest`.

### 2. Environment Differentiation & Secret Management
> [!WARNING]
> Never deploy hardcoded credentials (such as `DB_PASSWORD: postgres`) or plaintext JWT signing secrets directly to production AWS EKS.

* **Kustomize / Helm**: Define environment overlays (`base/`, `overlays/development/`, `overlays/production/`) or Helm values configuration to inject environment-specific configuration maps, database hosts, replica counts, and ingress routing rules dynamically.
* **Secret Vault Integration Service**:
  * For production, integrate with AWS Secrets Manager or HashiCorp Vault.
  * We recommend deploying the **External Secrets Operator (ESO)** in the EKS cluster. ESO fetches key material securely from AWS Secrets Manager and synchronizes them directly as Kubernetes Secrets (like `identity-signing-key`), keeping secret management completely separated from raw YAML repository commits.

### 3. Deploying Manifests
Once overlays or templated values are generated:
```bash
# E.g. using Kustomize overlays:
kubectl apply -k ../manifests/overlays/production/
```
