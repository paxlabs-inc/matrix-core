import { useState } from 'react';
import { NavLink, useParams } from 'react-router-dom';
import { ChevronDown, ChevronRight } from 'lucide-react';
import { docsNav } from '@/content';

function DomainSection({
  group,
  currentDomain,
  currentPage,
}: {
  group: (typeof docsNav)[0];
  currentDomain?: string;
  currentPage?: string;
}) {
  const isActive = currentDomain === group.id;
  const [isOpen, setIsOpen] = useState(isActive);

  return (
    <div className="mb-1">
      <button
        onClick={() => setIsOpen(!isOpen)}
        className="w-full flex items-center gap-1 px-4 py-1.5 text-xs font-medium text-fg-muted uppercase tracking-wider hover:text-fg-secondary transition-colors text-left"
      >
        {isOpen ? (
          <ChevronDown size={12} className="shrink-0" />
        ) : (
          <ChevronRight size={12} className="shrink-0" />
        )}
        {group.label}
      </button>
      {isOpen && (
        <div className="mt-0.5">
          {group.pages.map((page) => {
            const isPageActive = isActive && currentPage === page.slug;
            return (
              <NavLink
                key={page.slug}
                to={`/docs/${group.id}/${page.slug}`}
                className={`block px-4 py-1.5 text-sm transition-colors border-l-2 ${
                  isPageActive
                    ? 'text-fg-primary border-accent bg-accent-subtle'
                    : 'text-fg-secondary border-transparent hover:text-fg-primary hover:bg-bg-surface-hover'
                }`}
              >
                {page.title}
              </NavLink>
            );
          })}
        </div>
      )}
    </div>
  );
}

export default function Sidebar() {
  const { domain, page } = useParams<{ domain?: string; page?: string }>();

  return (
    <aside className="fixed left-0 top-[52px] bottom-0 w-[260px] bg-bg-base border-r border-border-subtle overflow-y-auto z-40 hidden lg:block">
      <nav className="py-4">
        {docsNav.map((group) => (
          <DomainSection
            key={group.id}
            group={group}
            currentDomain={domain}
            currentPage={page || 'INDEX'}
          />
        ))}
      </nav>
    </aside>
  );
}
