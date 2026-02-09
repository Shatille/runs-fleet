import type { Metadata } from 'next';
import './globals.css';
import AuthWrapper from '@/components/auth-wrapper';

export const metadata: Metadata = {
  title: 'runs-fleet Admin',
  description: 'Pool configuration management for runs-fleet',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body className="bg-gray-50 min-h-screen">
        <nav className="bg-white shadow-sm border-b">
          <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
            <div className="flex justify-between h-16">
              <div className="flex items-center">
                <a href="/admin/" className="text-xl font-semibold text-gray-900">
                  runs-fleet
                </a>
                <span className="ml-2 text-sm text-gray-500">Admin</span>
                <div className="ml-8 flex space-x-4">
                  <a href="/admin/" className="text-gray-600 hover:text-gray-900 px-3 py-2 rounded-md text-sm font-medium">
                    Pools
                  </a>
                  <a href="/admin/jobs/" className="text-gray-600 hover:text-gray-900 px-3 py-2 rounded-md text-sm font-medium">
                    Jobs
                  </a>
                  <a href="/admin/instances/" className="text-gray-600 hover:text-gray-900 px-3 py-2 rounded-md text-sm font-medium">
                    Instances
                  </a>
                  <a href="/admin/queues/" className="text-gray-600 hover:text-gray-900 px-3 py-2 rounded-md text-sm font-medium">
                    Queues
                  </a>
                  <a href="/admin/circuit/" className="text-gray-600 hover:text-gray-900 px-3 py-2 rounded-md text-sm font-medium">
                    Circuit
                  </a>
                </div>
              </div>
            </div>
          </div>
        </nav>
        <main className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
          <AuthWrapper>{children}</AuthWrapper>
        </main>
      </body>
    </html>
  );
}
