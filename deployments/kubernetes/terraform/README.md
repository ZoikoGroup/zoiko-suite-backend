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

### Deployment Flow
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
5. Deploy application manifests:
   ```bash
   kubectl apply -f ../manifests/
   ```
