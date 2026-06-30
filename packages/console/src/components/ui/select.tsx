import * as React from 'react'
import { ChevronDown } from 'lucide-react'
import { cn } from '~/lib/utils'

// Native-select wrapper styled to match the vendored shadcn primitives. Kept
// dependency-free (no Radix) like the rest of the vendored set.
function Select({ className, children, ...props }: React.ComponentProps<'select'>) {
  return (
    <div className="relative">
      <select
        className={cn(
          'border-input bg-background flex h-9 w-full appearance-none rounded-md border px-3 py-1 pr-8 text-sm shadow-xs outline-none',
          'focus-visible:ring-ring/50 focus-visible:ring-[3px]',
          'disabled:cursor-not-allowed disabled:opacity-50',
          className,
        )}
        {...props}
      >
        {children}
      </select>
      <ChevronDown className="text-muted-foreground pointer-events-none absolute top-1/2 right-2.5 size-4 -translate-y-1/2" />
    </div>
  )
}

export { Select }
