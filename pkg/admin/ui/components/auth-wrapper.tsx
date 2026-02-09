'use client';

import { useEffect, useState, ReactNode } from 'react';

interface AuthWrapperProps {
  children: ReactNode;
}

export default function AuthWrapper({ children }: AuthWrapperProps) {
  const [authError, setAuthError] = useState(false);

  useEffect(() => {
    const handleAuthRequired = () => setAuthError(true);
    window.addEventListener('auth-required', handleAuthRequired);
    return () => window.removeEventListener('auth-required', handleAuthRequired);
  }, []);

  if (authError) {
    return (
      <div className="max-w-md mx-auto mt-20">
        <div className="bg-white shadow rounded-lg p-6">
          <h2 className="text-xl font-semibold text-gray-900 mb-4">Authentication Required</h2>
          <p className="text-sm text-gray-600 mb-4">
            Your session has expired or you are not authenticated.
            Please log in through the authentication gateway.
          </p>
          <button
            onClick={() => window.location.reload()}
            className="w-full px-4 py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700"
          >
            Retry
          </button>
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
