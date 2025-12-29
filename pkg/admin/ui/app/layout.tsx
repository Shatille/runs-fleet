import type { Metadata } from 'next';
import './globals.css';

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
              </div>
            </div>
          </div>
        </nav>
        <main className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
          {children}
        </main>
      </body>
    </html>
  );
}
