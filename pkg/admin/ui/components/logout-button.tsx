'use client';

import { useAuthConfig } from '@/lib/use-auth-config';
import { useToast } from '@/components/toast';

export default function LogoutButton() {
  const authConfig = useAuthConfig();
  const { toast } = useToast();

  if (!authConfig?.auth_enabled) return null;

  async function handleLogout() {
    try {
      const res = await fetch('/api/auth/logout', { method: 'POST', credentials: 'include' });
      if (!res.ok) {
        toast('error', `Logout failed (${res.status}); please try again`);
        return;
      }
    } catch {
      toast('error', 'Logout request failed; please try again');
      return;
    }
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
