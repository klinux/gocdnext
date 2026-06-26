// Shared tab styling for the shadcn Tabs used across the app's section bars
// (compliance, run detail, …). Pill layout like the project nav — transparent
// track, content-width tabs — but the active tab carries the brand teal tint
// (instead of the neutral bg-accent default) for a clearer "you are here".
export const tabPillList = "h-auto bg-transparent p-0";

export const tabPillTrigger =
  "flex-none gap-1.5 px-3 py-1.5 text-muted-foreground data-active:border-primary/30 data-active:bg-primary/10 data-active:text-primary";
