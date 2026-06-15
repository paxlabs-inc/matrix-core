import { useEffect, useState, useCallback } from 'react';
import { useParams } from 'react-router-dom';
import { Copy, MessageSquare, Sparkles } from 'lucide-react';
import { getPage } from '@/content';

interface TocItem {
  id: string;
  text: string;
  level: number;
}

export default function RightSidebar() {
  const { domain, page } = useParams<{ domain?: string; page?: string }>();
  const [toc, setToc] = useState<TocItem[]>([]);
  const [activeId, setActiveId] = useState<string>('');

  const currentPage = domain ? getPage(domain, page) : undefined;
  const content = currentPage?.content;

  useEffect(() => {
    if (!content) {
      setToc([]);
      return;
    }

    const headings: TocItem[] = [];
    const lines = content.split('\n');
    for (const line of lines) {
      const match = line.match(/^(#{2,3})\s+(.+)$/);
      if (match) {
        const level = match[1].length;
        const text = match[2].replace(/\*\*/g, '').replace(/`/g, '');
        const id = text
          .toLowerCase()
          .replace(/[^a-z0-9\s-]/g, '')
          .replace(/\s+/g, '-');
        headings.push({ id, text, level });
      }
    }
    setToc(headings);
  }, [content]);

  useEffect(() => {
    if (toc.length === 0) return;

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setActiveId(entry.target.id);
          }
        }
      },
      { rootMargin: '-80px 0px -70% 0px' }
    );

    requestAnimationFrame(() => {
      for (const item of toc) {
        const el = document.getElementById(item.id);
        if (el) observer.observe(el);
      }
    });

    return () => observer.disconnect();
  }, [toc]);

  const handleClick = useCallback((id: string) => {
    const el = document.getElementById(id);
    if (el) {
      const y = el.getBoundingClientRect().top + window.scrollY - 80;
      window.scrollTo({ top: y, behavior: 'smooth' });
    }
  }, []);

  // Only show on doc pages when there are headings
  if (!domain) return null;

  return (
    <aside className="fixed right-0 top-[52px] bottom-0 w-[220px] bg-bg-base overflow-y-auto z-40 hidden xl:block py-6 px-4">
      {toc.length > 0 && (
        <>
          <div className="text-xs font-medium text-fg-muted uppercase tracking-wider mb-3">
            On this page
          </div>
          <nav className="space-y-0.5">
            {toc.map((item) => (
              <button
                key={item.id}
                onClick={() => handleClick(item.id)}
                className={`block w-full text-left text-sm py-1 transition-colors ${
                  item.level === 3 ? 'pl-3' : ''
                } ${
                  activeId === item.id
                    ? 'text-fg-primary'
                    : 'text-fg-muted hover:text-fg-primary'
                }`}
              >
                {item.text}
              </button>
            ))}
          </nav>
          <div className="border-t border-border-subtle my-4" />
        </>
      )}

      <div className="space-y-2">
        <button className="flex items-center gap-2 text-xs text-fg-muted hover:text-fg-primary transition-colors w-full text-left">
          <Copy size={13} />
          Copy page
        </button>
        <button className="flex items-center gap-2 text-xs text-fg-muted hover:text-fg-primary transition-colors w-full text-left">
          <MessageSquare size={13} />
          Share feedback
        </button>
        <button className="flex items-center gap-2 text-xs text-fg-muted hover:text-fg-primary transition-colors w-full text-left">
          <Sparkles size={13} />
          Explain more
        </button>
      </div>
    </aside>
  );
}
