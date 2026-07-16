export default function AuthCallbackLoading() {
  return (
    <main className="bg-background flex min-h-dvh items-center justify-center">
      <div
        className="border-muted-foreground/30 border-t-primary size-6 animate-spin rounded-full border-2"
        role="status"
        aria-label="Signing in"
      />
    </main>
  )
}
