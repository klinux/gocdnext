import { formatRelative } from "@/lib/format";

type Props = {
  at?: string | null;
  fallback?: string;
  className?: string;
};

export function RelativeTime({ at, fallback = "never", className }: Props) {
  if (!at) return <span className={className}>{fallback}</span>;
  return (
    <time dateTime={at} title={new Date(at).toLocaleString()} className={className}>
      {formatRelative(at)}
    </time>
  );
}
