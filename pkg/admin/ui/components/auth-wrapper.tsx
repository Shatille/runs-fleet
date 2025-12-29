'use client';

import { useEffect, useState, ReactNode } from 'react';
import { getAuthToken, setAuthToken, clearAuthToken } from '@/lib/api';

interface AuthWrapperProps {
  children: ReactNode;
}

export default function AuthWrapper({ children }: AuthWrapperProps) {
  const [showLogin, setShowLogin] = useState(false);
  const [token, setToken] = useState('');
  const [checking, setChecking] = useState(true);

  useEffect(() => {
    const handleAuthRequired = () => setShowLogin(true);
    window.addEventListener('auth-required', handleAuthRequired);

    // Check if we have a token and if auth is required
    const existingToken = getAuthToken();
    if (!existingToken) {
      // Try without auth first to see if it's enabled
      fetch('/api/pools')
        .then((res) => {
          if (res.status === 401) {
            setShowLogin(true);
          }
        })
        .finally(() => setChecking(false));
    } else {
      setChecking(false);
    }

    return () => window.removeEventListener('auth-required', handleAuthRequired);
  }, []);

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (token.trim()) {
      setAuthToken(token.trim());
      setShowLogin(false);
      window.location.reload();
    }
  }

  function handleLogout() {
    clearAuthToken();
    window.location.reload();
  }

  if (checking) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-gray-500">Loading...</div>
      </div>
    );
  }

  if (showLogin) {
    return (
      <div className="max-w-md mx-auto mt-20">
        <div className="bg-white shadow rounded-lg p-6">
          <h2 className="text-xl font-semibold text-gray-900 mb-4">Admin Authentication</h2>
          <p className="text-sm text-gray-600 mb-4">
            Enter the admin token to access the pool configuration interface.
          </p>
          <form onSubmit={handleSubmit}>
            <input
              type="password"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="Admin token"
              className="w-full px-3 py-2 border border-gray-300 rounded-md focus:outline-none focus:ring-2 focus:ring-blue-500"
              autoFocus
            />
            <button
              type="submit"
              className="w-full mt-4 px-4 py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700"
            >
              Authenticate
            </button>
          </form>
        </div>
      </div>
    );
  }

  return (
    <>
      {getAuthToken() && (
        <div className="fixed top-4 right-4">
          <button
            onClick={handleLogout}
            className="text-sm text-gray-500 hover:text-gray-700"
          >
            Logout
          </button>
        </div>
      )}
      {children}
    </>
  );
}
