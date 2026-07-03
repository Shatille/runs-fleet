'use client';

import { useAuthConfig } from '@/lib/use-auth-config';

export default function LogoutButton() {
  const authConfig = useAuthConfig();

  if (!authConfig?.auth_enabled) return null;

  async function handleLogout() {
    await fetch('/api/auth/logout', { method: 'POST', credentials: 'include' });
    window.location.href = '/admin/';
  }

  return (
    <button
      onClick={handleLogout}
      className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium"
    >
      Log out
    </button>
  );
}
