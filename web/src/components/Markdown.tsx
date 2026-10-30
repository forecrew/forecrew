import ReactMarkdown from "react-markdown"

/**
 * Renders untrusted markdown (agent output, GitHub text, summaries). Raw HTML
 * is never interpreted: react-markdown shows it as text, and no HTML plugin
 * is loaded. Images are dropped and links open in a new tab.
 */
export function Markdown({ children, className }: { children: string; className?: string }) {
  return (
    <div className={`markdown space-y-2 text-sm leading-relaxed break-words [&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_code]:font-mono [&_h1]:text-base [&_h1]:font-semibold [&_h2]:text-sm [&_h2]:font-semibold [&_h3]:text-sm [&_h3]:font-medium [&_ol]:list-decimal [&_ol]:pl-5 [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-3 [&_ul]:list-disc [&_ul]:pl-5 ${className ?? ""}`}>
      <ReactMarkdown
        disallowedElements={["img"]}
        components={{
          a: ({ href, children: c }) => (
            <a href={href} target="_blank" rel="noopener noreferrer" className="text-primary underline underline-offset-2">
              {c}
            </a>
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  )
}
