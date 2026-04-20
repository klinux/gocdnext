import { redirect } from "next/navigation";

// /settings is an alias for the Health tab so the sidebar link
// lands somewhere informative instead of a bare shell.
export default function SettingsIndexPage() {
  redirect("/settings/health");
}
