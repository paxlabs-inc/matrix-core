import { useCallback } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import remarkToc from 'remark-toc';
import rehypeSlug from 'rehype-slug';
import rehypeAutolinkHeadings from 'rehype-autolink-headings';
import rehypeHighlight from 'rehype-highlight';
import type { Components } from 'react-markdown';
import type { ReactNode } from 'react';
import CodeBlock from './CodeBlock';

interface MarkdownRendererProps {
  content: string;
}

interface CodeProps {
  children?: ReactNode;
  className?: string;
  node?: unknown;
  inline?: boolean;
}

export default function MarkdownRenderer({ content }: MarkdownRendererProps) {
  const components: Components = {
    code: useCallback(({ className, children, ...props }: CodeProps) => {
      const match = /language-(\w+)/.exec(className || '');
      const language = match ? match[1] : undefined;
      const codeString = String(children ?? '').replace(/\n$/, '');

      if (props.inline) {
        return (
          <code className="font-mono text-[13px] bg-bg-surface-active px-1.5 py-0.5 rounded-docs-sm text-fg-primary">
            {children}
          </code>
        );
      }

      return <CodeBlock code={codeString} language={language} />;
    }, []),

    h1: useCallback(({ children, id }: { children?: ReactNode; id?: string }) => (
      <h1 id={id} className="text-docs-3xl font-bold text-fg-primary mb-6 mt-0 tracking-tight">
        {children}
      </h1>
    ), []),

    h2: useCallback(({ children, id }: { children?: ReactNode; id?: string }) => (
      <h2 id={id} className="text-docs-xl font-semibold text-fg-primary mt-12 mb-4 tracking-tight">
        {children}
      </h2>
    ), []),

    h3: useCallback(({ children, id }: { children?: ReactNode; id?: string }) => (
      <h3 id={id} className="text-docs-lg font-semibold text-fg-primary mt-8 mb-3 tracking-tight">
        {children}
      </h3>
    ), []),

    h4: useCallback(({ children, id }: { children?: ReactNode; id?: string }) => (
      <h4 id={id} className="text-docs-base font-semibold text-fg-primary mt-6 mb-2">
        {children}
      </h4>
    ), []),

    p: useCallback(({ children }: { children?: ReactNode }) => (
      <p className="mb-4 leading-relaxed">{children}</p>
    ), []),

    a: useCallback(({ children, href }: { children?: ReactNode; href?: string }) => (
      <a
        href={href}
        className="text-fg-primary underline underline-offset-4 decoration-border-bright hover:decoration-fg-secondary transition-colors"
      >
        {children}
      </a>
    ), []),

    ul: useCallback(({ children }: { children?: ReactNode }) => (
      <ul className="list-disc pl-6 mb-4 space-y-1">{children}</ul>
    ), []),

    ol: useCallback(({ children }: { children?: ReactNode }) => (
      <ol className="list-decimal pl-6 mb-4 space-y-1">{children}</ol>
    ), []),

    li: useCallback(({ children }: { children?: ReactNode }) => (
      <li className="leading-relaxed">{children}</li>
    ), []),

    strong: useCallback(({ children }: { children?: ReactNode }) => (
      <strong className="text-fg-primary font-semibold">{children}</strong>
    ), []),

    blockquote: useCallback(({ children }: { children?: ReactNode }) => (
      <blockquote className="border-l-2 border-border-bright pl-4 my-6 text-fg-tertiary">
        {children}
      </blockquote>
    ), []),

    hr: useCallback(() => <hr className="border-border-subtle my-8" />, []),

    table: useCallback(({ children }: { children?: ReactNode }) => (
      <div className="overflow-x-auto my-6">
        <table className="w-full text-left text-docs-sm border-collapse">
          {children}
        </table>
      </div>
    ), []),

    thead: useCallback(({ children }: { children?: ReactNode }) => (
      <thead className="border-b border-border">{children}</thead>
    ), []),

    th: useCallback(({ children }: { children?: ReactNode }) => (
      <th className="py-2.5 px-3 font-medium text-fg-secondary">{children}</th>
    ), []),

    td: useCallback(({ children }: { children?: ReactNode }) => (
      <td className="py-2.5 px-3 border-b border-border-subtle">{children}</td>
    ), []),

    tbody: useCallback(({ children }: { children?: ReactNode }) => (
      <tbody>{children}</tbody>
    ), []),

    tr: useCallback(({ children }: { children?: ReactNode }) => (
      <tr className="hover:bg-bg-surface-hover transition-colors">{children}</tr>
    ), []),

    img: useCallback(({ src, alt }: { src?: string; alt?: string }) => (
      <img src={src} alt={alt} className="rounded-docs max-w-full my-6" />
    ), []),

    pre: useCallback(({ children }: { children?: ReactNode }) => <>{children}</>, []),
  };

  return (
    <div className="prose-docs">
      <ReactMarkdown
        remarkPlugins={[remarkGfm, [remarkToc, { heading: 'Table of Contents', maxDepth: 3 }]]}
        rehypePlugins={[
          rehypeSlug,
          [rehypeAutolinkHeadings, { behavior: 'wrap', test: ['h2', 'h3'] }],
          rehypeHighlight,
        ]}
        components={components}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
}
