import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";

import {
  BoolRow,
  CredentialsHeader,
  Field,
} from "./storage-form-fields";

// Panels are intentionally not "use client" — they're presentation
// over a Draft state owned by the parent. The parent passes setDraft
// directly so each control updates a single field without lifting
// every onChange through props.

export type Draft = {
  backend: "filesystem" | "s3" | "gcs";
  s3Bucket: string;
  s3Region: string;
  s3Endpoint: string;
  s3UsePathStyle: boolean;
  s3EnsureBucket: boolean;
  s3AccessKey: string;
  s3SecretKey: string;
  gcsBucket: string;
  gcsProjectID: string;
  gcsEnsureBucket: boolean;
  gcsServiceAccountJSON: string;
};

type PanelProps = {
  draft: Draft;
  setDraft: React.Dispatch<React.SetStateAction<Draft>>;
  credsConfigured: boolean;
  credsEditable: boolean;
  replaceCreds: boolean;
  setReplaceCreds: (v: boolean) => void;
};

export function S3Panel({
  draft,
  setDraft,
  credsConfigured,
  credsEditable,
  replaceCreds,
  setReplaceCreds,
}: PanelProps) {
  return (
    <div className="space-y-4">
      <div className="grid gap-4 md:grid-cols-2">
        <Field label="Bucket" required>
          <Input
            value={draft.s3Bucket}
            onValueChange={(v) => setDraft((d) => ({ ...d, s3Bucket: v }))}
            placeholder="gocdnext-artifacts"
            autoComplete="off"
          />
        </Field>
        <Field label="Region">
          <Input
            value={draft.s3Region}
            onValueChange={(v) => setDraft((d) => ({ ...d, s3Region: v }))}
            placeholder="us-east-1"
            autoComplete="off"
          />
        </Field>
        <Field
          label="Endpoint"
          hint="Leave empty for AWS. Set for MinIO / R2 / Backblaze."
          className="md:col-span-2"
        >
          <Input
            value={draft.s3Endpoint}
            onValueChange={(v) => setDraft((d) => ({ ...d, s3Endpoint: v }))}
            placeholder="https://s3.example.internal"
            autoComplete="off"
          />
        </Field>
      </div>

      <div className="flex flex-col gap-2">
        <BoolRow
          label="Use path-style URLs"
          hint="Required for MinIO and most non-AWS providers."
          checked={draft.s3UsePathStyle}
          onChange={(v) => setDraft((d) => ({ ...d, s3UsePathStyle: v }))}
        />
        <BoolRow
          label="Auto-create bucket"
          hint="Create the bucket on first use if it doesn't exist."
          checked={draft.s3EnsureBucket}
          onChange={(v) => setDraft((d) => ({ ...d, s3EnsureBucket: v }))}
        />
      </div>

      <CredentialsHeader
        configured={credsConfigured}
        replace={replaceCreds}
        onReplaceChange={setReplaceCreds}
        emptyHint="Leave empty to use IRSA / environment-provided credentials."
      />

      {credsEditable ? (
        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Access key ID">
            <Input
              value={draft.s3AccessKey}
              onValueChange={(v) => setDraft((d) => ({ ...d, s3AccessKey: v }))}
              placeholder="AKIA…"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field label="Secret access key">
            <Input
              type="password"
              value={draft.s3SecretKey}
              onValueChange={(v) => setDraft((d) => ({ ...d, s3SecretKey: v }))}
              placeholder="••••••••"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        </div>
      ) : null}
    </div>
  );
}

export function GCSPanel({
  draft,
  setDraft,
  credsConfigured,
  credsEditable,
  replaceCreds,
  setReplaceCreds,
}: PanelProps) {
  return (
    <div className="space-y-4">
      <div className="grid gap-4 md:grid-cols-2">
        <Field label="Bucket" required>
          <Input
            value={draft.gcsBucket}
            onValueChange={(v) => setDraft((d) => ({ ...d, gcsBucket: v }))}
            placeholder="gocdnext-artifacts"
            autoComplete="off"
          />
        </Field>
        <Field label="Project ID">
          <Input
            value={draft.gcsProjectID}
            onValueChange={(v) => setDraft((d) => ({ ...d, gcsProjectID: v }))}
            placeholder="my-gcp-project"
            autoComplete="off"
          />
        </Field>
      </div>

      <BoolRow
        label="Auto-create bucket"
        hint="Create the bucket on first use if it doesn't exist."
        checked={draft.gcsEnsureBucket}
        onChange={(v) => setDraft((d) => ({ ...d, gcsEnsureBucket: v }))}
      />

      <CredentialsHeader
        configured={credsConfigured}
        replace={replaceCreds}
        onReplaceChange={setReplaceCreds}
        emptyHint="Leave empty to use Workload Identity / GOOGLE_APPLICATION_CREDENTIALS."
      />

      {credsEditable ? (
        <Field
          label="Service account JSON"
          hint="Paste the full JSON key for a service account with Storage Object Admin on the bucket."
        >
          <Textarea
            value={draft.gcsServiceAccountJSON}
            onChange={(e) =>
              setDraft((d) => ({ ...d, gcsServiceAccountJSON: e.target.value }))
            }
            placeholder='{ "type": "service_account", ... }'
            rows={6}
            spellCheck={false}
            className="font-mono text-xs"
          />
        </Field>
      ) : null}
    </div>
  );
}
