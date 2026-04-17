import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { statusLabel, statusVariant } from "@/lib/status";

type Props = {
  status: string;
  className?: string;
};

export function StatusBadge({ status, className }: Props) {
  const variant = statusVariant(status);
  return (
    <Badge variant={variant} className={cn("capitalize", className)}>
      {statusLabel(status) || status}
    </Badge>
  );
}
