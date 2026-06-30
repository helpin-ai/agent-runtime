import * as React from 'react'
import { cn } from '~/lib/utils'

function Textarea({ className, ...props }: React.ComponentProps<'textarea'>) {
  return (
    <textarea
      className={cn(
        'border-input bg-background flex min-h-20 w-full rounded-md border px-3 py-2 text-sm shadow-xs transition-colors outline-none',
        'placeholder:text-muted-foreground focus-visible:ring-ring/50 focus-visible:ring-[3px]',
        'disabled:cursor-not-allowed disabled:opacity-50',
        className,
      )}
      {...props}
    />
  )
}

export { Textarea }
