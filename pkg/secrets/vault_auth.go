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
	case AuthMethodKubernetes, AuthMethodK8s, AuthMethodJWT:
		return authenticateWithJWT(ctx, client, cfg.K8sAuthMount, cfg.K8sRole, cfg.K8sJWTPath)
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

const awsSTSRegionalEndpointsEnv = "AWS_STS_REGIONAL_ENDPOINTS"

// globalSTSSigningRegion is the region the Vault AWS-IAM GetCallerIdentity is
// signed for. Vault validates the login against the global STS endpoint, which
// only accepts a us-east-1-scoped signature.
const globalSTSSigningRegion = "us-east-1"

func useGlobalSTSEndpoint() {
	// Force the legacy (global) STS endpoint so the signed host is
	// sts.amazonaws.com — the endpoint Vault replays validation to. Overrides an
	// inherited "regional" value, which would sign a regional host/scope that
	// Vault's global validation rejects ("Credential should be scoped to a valid
	// region"). Read only by AWS SDK v1 (the Vault auth helper); the agent's
	// SDK v2 clients ignore it.
	_ = os.Setenv(awsSTSRegionalEndpointsEnv, "legacy")
}

// authenticateAWS authenticates using AWS IAM credentials.
func authenticateAWS(ctx context.Context, client *api.Client, role, region string) error {
	useGlobalSTSEndpoint()

	// Sign the login for the global STS endpoint regardless of the operating
	// region; `region` still scopes real AWS API calls elsewhere.
	opts := []aws.LoginOption{
		aws.WithIAMAuth(),
		aws.WithRegion(globalSTSSigningRegion),
	}

	if role != "" {
		opts = append(opts, aws.WithRole(role))
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

// authenticateWithJWT authenticates using a JWT token.
// This works for both Vault's Kubernetes and JWT auth methods as they share the same API format.
func authenticateWithJWT(ctx context.Context, client *api.Client, authMount, role, jwtPath string) error {
	if authMount == "" {
		authMount = "kubernetes"
	}
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

	loginPath := fmt.Sprintf("auth/%s/login", authMount)
	secret, err := client.Logical().WriteWithContext(ctx, loginPath, data)
	if err != nil {
		return fmt.Errorf("kubernetes auth login failed (mount=%s, role=%s): %w", authMount, role, err)
	}

	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("kubernetes auth returned no auth info (mount=%s, role=%s)", authMount, role)
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
