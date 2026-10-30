export function PageHeader({
  title, badge, meta, children,
}: {
  title: string
  /** Next to the title, e.g. a status. */
  badge?: React.ReactNode
  /** A line under the title. */
  meta?: React.ReactNode
  children?: React.ReactNode
}) {
  return (
    <div className="mb-6 flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
          {badge}
        </div>
        {meta ? <p className="text-sm text-muted-foreground">{meta}</p> : null}
      </div>
      {children ? <div className="flex flex-wrap items-center gap-2">{children}</div> : null}
    </div>
  )
}
