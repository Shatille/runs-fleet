'use client';

import { useAuthConfig } from '@/lib/use-auth-config';
import { useToast } from '@/components/toast';

export default function LogoutButton() {
  const authConfig = useAuthConfig();
  const { toast } = useToast();

  if (!authConfig?.auth_enabled) return null;

  // Only navigate away on a confirmed 2xx: the session cookie is cleared
  // server-side, so navigating after a failed request would leave the user
  // believing they're logged out while the cookie (and server session) is
  // still valid.
  async function handleLogout() {
    let succeeded = false;
    try {
      const res = await fetch('/api/auth/logout', { method: 'POST', credentials: 'include' });
      succeeded = res.ok;
      if (!succeeded) {
        toast('error', `Logout failed (${res.status}); please try again`);
      }
    } catch {
      toast('error', 'Logout request failed; please try again');
    }
    if (succeeded) {
      window.location.href = '/admin/';
    }
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
