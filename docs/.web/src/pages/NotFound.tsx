import { Link } from 'react-router-dom';

export default function NotFound() {
  return (
    <div className="text-center py-20">
      <h1 className="text-docs-3xl font-bold text-fg-primary mb-4">404</h1>
      <p className="text-fg-secondary mb-6">This page does not exist.</p>
      <Link
        to="/"
        className="text-fg-primary underline underline-offset-4 decoration-border-bright hover:decoration-fg-secondary transition-colors"
      >
        Go back home
      </Link>
    </div>
  );
}
