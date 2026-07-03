'use client';

import { useEffect, useState, ReactNode } from 'react';
import { useAuthConfig } from '@/lib/use-auth-config';

interface AuthWrapperProps {
  children: ReactNode;
}

export default function AuthWrapper({ children }: AuthWrapperProps) {
  const [authError, setAuthError] = useState(false);
  const authConfig = useAuthConfig();

  useEffect(() => {
    const handleAuthRequired = () => setAuthError(true);
    window.addEventListener('auth-required', handleAuthRequired);
    return () => window.removeEventListener('auth-required', handleAuthRequired);
  }, []);

  if (authError) {
    const loginURL =
      authConfig?.auth_enabled && authConfig.login_url
        ? `${authConfig.login_url}?redirect=${encodeURIComponent(window.location.pathname)}`
        : null;

    return (
      <div className="max-w-md mx-auto mt-20">
        <div className="bg-white dark:bg-gray-800 shadow rounded-lg p-6">
          <h2 className="text-xl font-semibold text-gray-900 dark:text-gray-100 mb-4">Authentication Required</h2>
          <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
            {loginURL
              ? 'Your session has expired or you are not authenticated. Please log in to continue.'
              : 'Your session has expired or you are not authenticated. Please log in through the authentication gateway.'}
          </p>
          {loginURL ? (
            <a
              href={loginURL}
              className="block w-full text-center px-4 py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700"
            >
              Log In
            </a>
          ) : (
            <button
              onClick={() => window.location.reload()}
              className="w-full px-4 py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700"
            >
              Retry
            </button>
          )}
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
