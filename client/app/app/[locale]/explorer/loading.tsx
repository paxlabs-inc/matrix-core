export default function ExplorerLoading() {
  return (
    <div className="flex flex-col gap-3">
      {Array.from({ length: 4 }).map((_, i) => (
        <div key={i} className="bg-card h-24 animate-pulse rounded-xl" />
      ))}
    </div>
  )
}
