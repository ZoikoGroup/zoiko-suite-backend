output "cluster_endpoint" {
  description = "Endpoint for EKS control plane"
  value       = aws_eks_cluster.eks.endpoint
}

output "cluster_security_group_id" {
  description = "Security group IDs attached to the cluster control plane"
  value       = aws_eks_cluster.eks.vpc_config[0].cluster_security_group_id
}

output "cluster_name" {
  description = "Kubernetes Cluster Name"
  value       = aws_eks_cluster.eks.name
}

output "oidc_provider_arn" {
  description = "The ARN of the OIDC Provider for Workload Identity (IRSA)"
  value       = aws_iam_openid_connect_provider.oidc.arn
}

output "kubeconfig_connect_command" {
  description = "AWS CLI command to update local kubeconfig with the EKS cluster context"
  value       = "aws eks update-kubeconfig --region ${var.aws_region} --name ${aws_eks_cluster.eks.name}"
}
