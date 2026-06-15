import { useState, useCallback } from 'react';
import { Check, Clipboard } from 'lucide-react';

interface CodeBlockProps {
  code: string;
  language?: string;
}

export default function CodeBlock({ code, language }: CodeBlockProps) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(code).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  }, [code]);

  return (
    <div className="bg-bg-code border border-border-subtle rounded-docs overflow-hidden my-6">
      {language && (
        <div className="bg-bg-surface px-4 py-2 flex justify-between items-center border-b border-border-subtle">
          <span className="text-xs text-fg-muted uppercase">{language}</span>
          <button
            onClick={handleCopy}
            className="text-fg-muted hover:text-fg-primary transition-colors"
            aria-label="Copy code"
          >
            {copied ? <Check size={14} /> : <Clipboard size={14} />}
          </button>
        </div>
      )}
      <div className="p-4 overflow-x-auto relative">
        {!language && (
          <button
            onClick={handleCopy}
            className="absolute top-2 right-2 text-fg-muted hover:text-fg-primary transition-colors z-10"
            aria-label="Copy code"
          >
            {copied ? <Check size={14} /> : <Clipboard size={14} />}
          </button>
        )}
        <pre className="text-[13px] leading-relaxed font-mono">
          <code>{code}</code>
        </pre>
      </div>
    </div>
  );
}
