/** Lets a wide table scroll sideways on a phone; keyboard users can scroll it too. */
export function TableWrap({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div role="region" aria-label={`${label} table`} tabIndex={0} className="overflow-x-auto rounded-xl border bg-card [&_[data-slot=table-container]]:overflow-visible">
      {children}
    </div>
  )
}
