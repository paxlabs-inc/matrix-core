import { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { FileText, Search } from 'lucide-react';
import Fuse from 'fuse.js';
import { allPages } from '@/content';

interface SearchModalProps {
  isOpen: boolean;
  onClose: () => void;
}

export default function SearchModal({ isOpen, onClose }: SearchModalProps) {
  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();

  const fuse = useMemo(
    () =>
      new Fuse(allPages, {
        keys: ['title', 'domainLabel', 'content'],
        threshold: 0.4,
        ignoreLocation: true,
      }),
    []
  );

  const results = useMemo(() => {
    if (!query.trim()) return [];
    return fuse.search(query).map((r) => r.item);
  }, [query, fuse]);

  useEffect(() => {
    setActiveIndex(0);
  }, [query]);

  useEffect(() => {
    if (isOpen) {
      setQuery('');
      setActiveIndex(0);
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [isOpen]);

  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault();
        if (isOpen) onClose();
        else;
      }
      if (!isOpen) return;

      if (e.key === 'Escape') {
        onClose();
        return;
      }
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setActiveIndex((i) => Math.min(i + 1, results.length - 1));
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        setActiveIndex((i) => Math.max(i - 1, 0));
      } else if (e.key === 'Enter' && results[activeIndex]) {
        e.preventDefault();
        const r = results[activeIndex];
        navigate(`/docs/${r.domain}/${r.slug}`);
        onClose();
      }
    }
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, onClose, results, activeIndex, navigate]);

  const handleSelect = useCallback(
    (domain: string, slug: string) => {
      navigate(`/docs/${domain}/${slug}`);
      onClose();
    },
    [navigate, onClose]
  );

  if (!isOpen) return null;

  return (
    <div
      className="fixed inset-0 bg-black/60 backdrop-blur-sm z-[100] flex items-start justify-center pt-[15vh]"
      onClick={onClose}
    >
      <div
        className="w-full max-w-xl bg-bg-surface border border-border rounded-docs shadow-2xl overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Input */}
        <div className="flex items-center gap-3 px-4 border-b border-border-subtle">
          <Search size={18} className="text-fg-muted shrink-0" />
          <input
            ref={inputRef}
            type="text"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search documentation..."
            className="flex-1 bg-transparent py-4 text-lg text-fg-primary placeholder:text-fg-muted outline-none"
          />
        </div>

        {/* Results */}
        <div className="max-h-96 overflow-y-auto">
          {results.length === 0 && query.trim() && (
            <div className="p-8 text-center text-fg-muted text-sm">No results found</div>
          )}
          {results.length === 0 && !query.trim() && (
            <div className="p-8 text-center text-fg-muted text-sm">
              Type to search documentation...
            </div>
          )}
          {results.map((r, i) => (
            <button
              key={`${r.domain}-${r.slug}`}
              onClick={() => handleSelect(r.domain, r.slug)}
              onMouseEnter={() => setActiveIndex(i)}
              className={`w-full flex items-center gap-3 p-3 text-left transition-colors cursor-pointer ${
                i === activeIndex
                  ? 'bg-bg-surface-hover border-l-2 border-accent'
                  : 'hover:bg-bg-surface-hover border-l-2 border-transparent'
              }`}
            >
              <FileText size={16} className="text-fg-muted shrink-0" />
              <div className="flex-1 min-w-0">
                <div className="text-sm text-fg-primary truncate">{r.title}</div>
                <div className="text-xs text-fg-muted">
                  {r.domainLabel}
                </div>
              </div>
            </button>
          ))}
        </div>

        {/* Footer */}
        <div className="flex items-center gap-4 px-4 py-2.5 border-t border-border-subtle text-[11px] text-fg-muted">
          <span>
            <kbd className="font-mono border border-border rounded px-1">↑↓</kbd> navigate
          </span>
          <span>
            <kbd className="font-mono border border-border rounded px-1">↵</kbd> select
          </span>
          <span>
            <kbd className="font-mono border border-border rounded px-1">esc</kbd> close
          </span>
        </div>
      </div>
    </div>
  );
}
