import { useParams, Link } from 'react-router-dom';
import { ArrowLeft, ArrowRight } from 'lucide-react';
import { getPage, getAdjacentPage } from '@/content';
import MarkdownRenderer from '@/components/MarkdownRenderer';

export default function DocsPage() {
  const { domain, page } = useParams<{ domain: string; page?: string }>();

  const currentPage = getPage(domain ?? '', page);
  const { prev, next } = getAdjacentPage(domain ?? '', page || 'INDEX');

  const domainLabel = domain
    ? domain.charAt(0).toUpperCase() + domain.slice(1)
    : '';

  if (!currentPage) {
    return (
      <div className="text-center py-20">
        <h1 className="text-docs-2xl font-bold text-fg-primary mb-4">Page not found</h1>
        <p className="text-fg-secondary mb-6">
          The page you are looking for does not exist.
        </p>
        <Link to="/" className="text-accent hover:text-accent-hover underline">
          Go home
        </Link>
      </div>
    );
  }

  return (
    <div>
      {/* Breadcrumb */}
      <div className="flex items-center gap-2 text-docs-xs text-fg-muted mb-6">
        <Link to="/" className="hover:text-fg-secondary transition-colors">
          Docs
        </Link>
        <span>/</span>
        <span className="text-fg-secondary">{domainLabel}</span>
        {page && page !== 'INDEX' && page !== 'index' && (
          <>
            <span>/</span>
            <span className="text-fg-primary">{currentPage.title}</span>
          </>
        )}
      </div>

      {/* Content */}
      <MarkdownRenderer content={currentPage.content} />

      {/* Prev/Next navigation */}
      <div className="border-t border-border-subtle mt-12 pt-6 flex items-center justify-between">
        {prev ? (
          <Link
            to={`/docs/${domain}/${prev.slug}`}
            className="flex items-center gap-2 text-sm text-fg-secondary hover:text-fg-primary transition-colors"
          >
            <ArrowLeft size={16} />
            <div>
              <div className="text-fg-muted text-xs">Previous</div>
              <div>{prev.title}</div>
            </div>
          </Link>
        ) : (
          <div />
        )}
        {next ? (
          <Link
            to={`/docs/${domain}/${next.slug}`}
            className="flex items-center gap-2 text-sm text-fg-secondary hover:text-fg-primary transition-colors text-right"
          >
            <div>
              <div className="text-fg-muted text-xs">Next</div>
              <div>{next.title}</div>
            </div>
            <ArrowRight size={16} />
          </Link>
        ) : (
          <div />
        )}
      </div>
    </div>
  );
}
