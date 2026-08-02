// runs.queue_reason is a free-form `key:detail` string (server migration
// 00034), so the UI parses it rather than switching on an enum — a new producer
// on the server must not require a frontend release to avoid rendering garbage.
//
// Known keys today:
//   serial-busy:<run-id>       the pipeline is serial and a predecessor is live
//   frozen-deploy:<env>        an environment change-freeze is holding a job
//                              targeting <env> — a deploy OR a migration (#202, #206)
//   supersede-blocked:<env>    a newer run in the lane already cleared this env
//   supersede-lock-busy:<env>  the lane-env lock is contended this tick

export type QueueReason = {
  /** Short label for the pill, e.g. "Frozen: production". */
  label: string;
  /** Longer explanation for the pill's tooltip. */
  title: string;
};

/**
 * formatQueueReason turns a raw queue_reason into a renderable label, or null
 * when there is nothing worth showing.
 *
 * Returns null (rather than echoing the raw string) for unknown keys: a future
 * server-side producer would otherwise leak an internal token like
 * `some-new-gate:9f3e…` into the UI, which reads as a bug to an operator. A
 * missing pill is a strictly better failure than a meaningless one.
 */
export function formatQueueReason(reason?: string): QueueReason | null {
  if (!reason) return null;
  const sep = reason.indexOf(":");
  if (sep <= 0) return null;
  const key = reason.slice(0, sep);
  const detail = reason.slice(sep + 1).trim();
  if (detail === "") return null;

  switch (key) {
    case "frozen-deploy":
      return {
        label: `Frozen: ${detail}`,
        // Deliberately explicit that this is not a whole-run halt: a stage can
        // hold a frozen `production` job while its `staging` sibling runs, and an
        // operator reading "Frozen" alone would conclude otherwise. "jobs" (not
        // "deploy"): the hold covers any job targeting the env — a deploy OR a
        // migration (#206) — and there may be several targeting the same env.
        title: `A change-freeze on ${detail} is holding jobs in this run that target it. Jobs targeting other environments are unaffected.`,
      };
    case "supersede-blocked":
      return {
        label: `Superseded: ${detail}`,
        title: `A newer run in this lane already cleared the gate for ${detail}.`,
      };
    default:
      return null;
  }
}
