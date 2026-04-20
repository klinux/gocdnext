"use client"

import { Separator as SeparatorPrimitive } from "@base-ui/react/separator"

import { cn } from "@/lib/utils"

function Separator({
  className,
  orientation = "horizontal",
  ...props
}: SeparatorPrimitive.Props) {
  return (
    <SeparatorPrimitive
      data-slot="separator"
      orientation={orientation}
      // No self-stretch on vertical: it overrode the parent's
      // `items-center` and pinned the bar to the top of flex
      // headers. Callers pass an explicit h-N (all current call
      // sites do) — the 1px-wide rule then aligns with sibling
      // content instead of hugging the top edge.
      className={cn(
        "shrink-0 bg-border data-horizontal:h-px data-horizontal:w-full data-vertical:w-px",
        className
      )}
      {...props}
    />
  )
}

export { Separator }
