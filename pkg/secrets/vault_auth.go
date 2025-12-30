package secrets

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/aws"
)

// authenticate sets up Vault authentication based on config.
func authenticate(ctx context.Context, client *api.Client, cfg VaultConfig) error {
	switch cfg.AuthMethod {
	case AuthMethodAWS:
		return authenticateAWS(ctx, client, cfg.AWSRole, cfg.AWSRegion)
	case AuthMethodKubernetes, AuthMethodK8s:
		return authenticateK8s(ctx, client, cfg.K8sRole, cfg.K8sJWTPath)
	case AuthMethodAppRole:
		return authenticateAppRole(ctx, client, cfg.AppRoleID, cfg.AppRoleSecretID)
	case AuthMethodToken:
		if cfg.Token != "" {
			client.SetToken(cfg.Token)
		}
		return nil
	case "":
		// If no auth method specified, try token from env (VAULT_TOKEN)
		if token := os.Getenv("VAULT_TOKEN"); token != "" {
			client.SetToken(token)
			return nil
		}
		// Default to AWS auth if running in AWS environment
		return authenticateAWS(ctx, client, cfg.AWSRole, cfg.AWSRegion)
	default:
		return fmt.Errorf("unsupported auth method: %s", cfg.AuthMethod)
	}
}

// authenticateAWS authenticates using AWS IAM credentials.
func authenticateAWS(ctx context.Context, client *api.Client, role, region string) error {
	opts := []aws.LoginOption{
		aws.WithIAMAuth(),
	}

	if role != "" {
		opts = append(opts, aws.WithRole(role))
	}

	if region != "" {
		opts = append(opts, aws.WithRegion(region))
	}

	awsAuth, err := aws.NewAWSAuth(opts...)
	if err != nil {
		return fmt.Errorf("failed to create AWS auth (role=%s, region=%s): %w", role, region, err)
	}

	authInfo, err := client.Auth().Login(ctx, awsAuth)
	if err != nil {
		return fmt.Errorf("AWS auth login failed (role=%s, region=%s): %w", role, region, err)
	}

	if authInfo == nil {
		return fmt.Errorf("AWS auth returned no info (role=%s)", role)
	}

	return nil
}

// authenticateK8s authenticates using Kubernetes service account token.
func authenticateK8s(ctx context.Context, client *api.Client, role, jwtPath string) error {
	if jwtPath == "" {
		jwtPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}

	jwt, err := os.ReadFile(jwtPath)
	if err != nil {
		return fmt.Errorf("failed to read Kubernetes JWT from %s: %w", jwtPath, err)
	}

	data := map[string]interface{}{
		"role": role,
		"jwt":  string(jwt),
	}

	secret, err := client.Logical().WriteWithContext(ctx, "auth/kubernetes/login", data)
	if err != nil {
		return fmt.Errorf("kubernetes auth login failed (role=%s): %w", role, err)
	}

	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("kubernetes auth returned no auth info (role=%s)", role)
	}

	client.SetToken(secret.Auth.ClientToken)
	return nil
}

// authenticateAppRole authenticates using AppRole credentials.
func authenticateAppRole(ctx context.Context, client *api.Client, roleID, secretID string) error {
	data := map[string]interface{}{
		"role_id":   roleID,
		"secret_id": secretID,
	}

	secret, err := client.Logical().WriteWithContext(ctx, "auth/approle/login", data)
	if err != nil {
		return fmt.Errorf("AppRole auth login failed: %w", err)
	}

	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("AppRole auth returned no auth info")
	}

	client.SetToken(secret.Auth.ClientToken)
	return nil
}
