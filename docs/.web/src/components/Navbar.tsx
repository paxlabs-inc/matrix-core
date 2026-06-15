import { Search } from 'lucide-react';

interface NavbarProps {
  onSearchClick: () => void;
}

export default function Navbar({ onSearchClick }: NavbarProps) {
  return (
    <header className="fixed top-0 left-0 right-0 h-[52px] bg-bg-base border-b border-border-subtle z-50 flex items-center px-4">
      {/* Left: Logo + Nav tabs */}
      <div className="flex items-center gap-6">
        <a href="#/" className="flex items-center gap-2 select-none">
          <span className="text-fg-primary font-bold text-sm tracking-widest">MATRIX</span>
        </a>
        <nav className="hidden md:flex items-center gap-4">
          <a href="#/" className="text-fg-primary text-sm font-medium transition-colors">
            Docs
          </a>
          <span className="text-fg-muted text-sm cursor-default">API</span>
          <span className="text-fg-muted text-sm cursor-default">Learn</span>
          <span className="text-fg-muted text-sm cursor-default">Help</span>
        </nav>
      </div>

      {/* Center: Search bar */}
      <div className="flex-1 flex justify-center px-4">
        <button
          onClick={onSearchClick}
          className="flex items-center gap-2 bg-bg-surface border border-border rounded-lg px-3 py-1.5 w-full max-w-[280px] hover:border-border-bright transition-colors text-left"
        >
          <Search size={14} className="text-fg-muted shrink-0" />
          <span className="text-fg-muted text-sm flex-1">Search docs...</span>
          <kbd className="text-[11px] text-fg-muted border border-border rounded px-1.5 py-0.5 font-mono shrink-0">
            ⌘K
          </kbd>
        </button>
      </div>

      {/* Right: Auth + Download */}
      <div className="flex items-center gap-3">
        <span className="text-fg-secondary hover:text-fg-primary text-sm cursor-pointer transition-colors hidden sm:block">
          Sign in
        </span>
        <button className="bg-white text-black font-medium text-sm px-4 py-1.5 rounded-full hover:bg-gray-100 transition-colors">
          Download
        </button>
      </div>
    </header>
  );
}
