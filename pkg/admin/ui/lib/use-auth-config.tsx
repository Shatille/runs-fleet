'use client';

import { createContext, useContext, useEffect, useState, ReactNode } from 'react';

export interface AuthConfig {
  auth_enabled: boolean;
  login_url?: string;
}

const AuthConfigContext = createContext<AuthConfig | null>(null);

// AuthConfigProvider fetches whether OIDC auth is configured once per page
// load and shares it with every consumer, instead of each caller (the auth
// wrapper, the logout button) independently fetching /api/auth/config.
export function AuthConfigProvider({ children }: { children: ReactNode }) {
  const [config, setConfig] = useState<AuthConfig | null>(null);

  useEffect(() => {
    let cancelled = false;

    fetch('/api/auth/config', { credentials: 'include' })
      .then((res) => res.json())
      .then((data: AuthConfig) => {
        if (!cancelled) setConfig(data);
      })
      .catch(() => {
        if (!cancelled) setConfig({ auth_enabled: false });
      });

    return () => {
      cancelled = true;
    };
  }, []);

  return <AuthConfigContext.Provider value={config}>{children}</AuthConfigContext.Provider>;
}

// useAuthConfig returns whether OIDC auth is enabled, so components can
// render a login/logout affordance without hardcoding deployment
// assumptions. Returns null until the provider's first response arrives.
export function useAuthConfig(): AuthConfig | null {
  return useContext(AuthConfigContext);
}
