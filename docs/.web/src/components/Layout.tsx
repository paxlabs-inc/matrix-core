import { useState, useCallback, useEffect } from 'react';
import { Outlet } from 'react-router-dom';
import Navbar from './Navbar';
import Sidebar from './Sidebar';
import RightSidebar from './RightSidebar';
import SearchModal from './SearchModal';

export default function Layout() {
  const [searchOpen, setSearchOpen] = useState(false);

  const handleSearchClick = useCallback(() => {
    setSearchOpen(true);
  }, []);

  const handleSearchClose = useCallback(() => {
    setSearchOpen(false);
  }, []);

  // Global Cmd+K shortcut
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault();
        setSearchOpen((prev) => !prev);
      }
    }
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, []);

  return (
    <div className="min-h-screen bg-bg-base">
      <Navbar onSearchClick={handleSearchClick} />
      <Sidebar />
      <SearchModal isOpen={searchOpen} onClose={handleSearchClose} />

      {/* Main content area */}
      <main className="pt-[52px] lg:ml-[260px] xl:mr-[220px]">
        <div className="max-w-docs mx-auto px-6 py-10">
          <Outlet />
        </div>
      </main>

      <RightSidebar />
    </div>
  );
}
