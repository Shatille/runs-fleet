import type { Metadata } from 'next';
import './globals.css';
import AuthWrapper from '@/components/auth-wrapper';
import DarkModeToggle from '@/components/dark-mode-toggle';
import { ToastProvider } from '@/components/toast';

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
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: `
          (function() {
            var t = localStorage.getItem('theme');
            if (t === 'dark' || (!t && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
              document.documentElement.classList.add('dark');
            }
          })();
        `}} />
      </head>
      <body className="bg-gray-50 dark:bg-gray-900 min-h-screen transition-colors">
        <ToastProvider>
          <nav className="bg-white dark:bg-gray-800 shadow-sm border-b dark:border-gray-700">
            <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
              <div className="flex justify-between h-16">
                <div className="flex items-center">
                  <a href="/admin/" className="text-xl font-semibold text-gray-900 dark:text-gray-100">
                    runs-fleet
                  </a>
                  <span className="ml-2 text-sm text-gray-500 dark:text-gray-400">Admin</span>
                  <div className="ml-8 flex space-x-4">
                    <a href="/admin/" className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium">
                      Pools
                    </a>
                    <a href="/admin/jobs/" className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium">
                      Jobs
                    </a>
                    <a href="/admin/instances/" className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium">
                      Instances
                    </a>
                    <a href="/admin/queues/" className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium">
                      Queues
                    </a>
                    <a href="/admin/circuit/" className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium">
                      Circuit
                    </a>
                    <a href="/admin/cost/" className="text-gray-600 dark:text-gray-300 hover:text-gray-900 dark:hover:text-white px-3 py-2 rounded-md text-sm font-medium">
                      Cost
                    </a>
                  </div>
                </div>
                <div className="flex items-center">
                  <DarkModeToggle />
                </div>
              </div>
            </div>
          </nav>
          <main className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
            <AuthWrapper>{children}</AuthWrapper>
          </main>
        </ToastProvider>
      </body>
    </html>
  );
}
