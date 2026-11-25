"use client";

import { useState } from "react";

import { FormField } from "@/components/form/form-field";
import { PageHeader } from "@/components/layout/page-header";
import { Button } from "@/components/ui/button";
import { CopyLine } from "@/components/ui/copy-line";
import { DialogFrame } from "@/components/ui/dialog-frame";
import { ErrorText } from "@/components/ui/error-text";
import { Icon } from "@/components/ui/icon";
import { TextInput } from "@/components/ui/text-input";
import { post, remove } from "@/lib/api/http";
import { identityMaximum, identityNamePattern, loadIdentities, tokenCommand } from "@/lib/api/identities";
import type { Identity } from "@/lib/api/types";
import { formatTime } from "@/lib/formatting/format";
import { useAction } from "@/lib/hooks/use-action";
import { useResource } from "@/lib/hooks/use-resource";

const NewIdentity = ({ count, onAdded }: { readonly count: number; readonly onAdded: () => Promise<void> }) => {
  const action = useAction();
  const [name, setName] = useState("");
  const full = count >= identityMaximum;
  const valid = identityNamePattern.test(name);
  return (
    <form
      className="flex flex-col gap-4 rounded-lg border border-border bg-surface p-4"
      onSubmit={(event) => {
        event.preventDefault();
        if (!valid || full) {
          return;
        }
        void action.run(() => post<Identity>("/api/identities", { name })).then(async (done) => {
          if (done) {
            setName("");
            await onAdded();
          }
        });
      }}
    >
      <FormField
        label="Name"
        htmlFor="identity-name"
        hint="Lowercase letters, digits and hyphens. Machines can wear it from when they're made, or from their next start."
      >
        <TextInput
          id="identity-name"
          value={name}
          maxLength={63}
          onChange={(event) => setName(event.target.value.toLowerCase())}
          placeholder="build"
          autoComplete="off"
          aria-invalid={(name !== "" && !valid) || undefined}
        />
      </FormField>
      <div className="flex flex-wrap items-center gap-3">
        <Button type="submit" variant="primary" disabled={!valid || full} pending={action.pending} leadingIcon={<Icon name="plus" size={12} />}>
          Make identity
        </Button>
        {full ? <span className="text-xs text-foreground-tertiary">You have {identityMaximum}, the most allowed.</span> : null}
        <ErrorText message={action.error} />
      </div>
    </form>
  );
};

const DeleteIdentity = ({
  identity,
  onClose,
  onDeleted,
}: {
  readonly identity: Identity;
  readonly onClose: () => void;
  readonly onDeleted: () => Promise<void>;
}) => {
  const action = useAction();
  return (
    <DialogFrame
      title={`Delete the identity ${identity.name}?`}
      onClose={onClose}
      submitLabel="Delete identity"
      danger
      pending={action.pending}
      error={action.error}
      onSubmit={() =>
        void action.run(() => remove(`/api/identities/${encodeURIComponent(identity.account)}`)).then(async (done) => {
          if (done) {
            onClose();
            await onDeleted();
          }
        })
      }
    >
      <p className="text-[13px] text-foreground-secondary">
        What was granted to it elsewhere stops working, and it can&apos;t be had again: a new identity of the same name is
        another one, with another subject.
      </p>
    </DialogFrame>
  );
};

/** Identities: what machines are to other services. */
export const IdentitiesPage = () => {
  const identities = useResource(loadIdentities);
  const [deleting, setDeleting] = useState<Identity | null>(null);
  const mine = identities.data?.filter((identity) => identity.mine).length ?? 0;
  return (
    <>
      <PageHeader title="Identities" icon={<Icon name="shield" size={15} />} />
      <div className="scrollbar-thin min-h-0 min-w-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex max-w-[880px] flex-col gap-6 px-4 py-5 md:px-8 md:py-8">
          <div className="flex flex-col gap-2 text-xs text-foreground-tertiary">
            <p>
              An identity is what a machine is to other services, such as the drive: grant something to its issuer and
              subject there, and every machine that wears it gets it. Every machine wears one: its own, named after it, unless
              you pick another when you make it. Several machines can wear one and share what it was granted.
            </p>
            <p>On a machine, ask for a token made out to the service you&apos;ll show it to (valid one hour):</p>
            <CopyLine text={tokenCommand("https://files.drive.syeyoung.dev")} />
          </div>

          <section className="flex flex-col gap-3">
            <h2 className="text-[13px] font-medium text-foreground">Your identities</h2>
            <ErrorText message={identities.error} />
            {identities.data === null ? (
              identities.error ? null : <div className="py-6 text-center text-xs text-foreground-tertiary">Loading…</div>
            ) : identities.data.length === 0 ? (
              <p className="rounded-lg border border-border bg-background-secondary px-4 py-6 text-center text-xs text-foreground-tertiary">
                No identities yet. Each new machine gets its own.
              </p>
            ) : (
              <ul className="flex flex-col divide-y divide-border rounded-lg border border-border">
                {identities.data.map((identity) => (
                  <li key={identity.account} className="flex flex-col gap-2 px-4 py-3">
                    <div className="flex items-center gap-3">
                      <Icon name="shield" size={14} className="shrink-0 text-foreground-tertiary" />
                      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                        <span className="truncate text-[13px] text-foreground">
                          {identity.name}
                          {identity.mine ? null : <span className="text-foreground-tertiary"> · {identity.owner}</span>}
                        </span>
                        <span className="truncate text-2xs text-foreground-tertiary">
                          {identity.usedBy.length === 0 ? "No machine wears it" : `Worn by ${identity.usedBy.join(", ")}`}
                        </span>
                      </div>
                      <span className="hidden shrink-0 text-xs text-foreground-tertiary sm:inline">{formatTime(identity.createdAt)}</span>
                      <Button
                        variant="ghost"
                        size="small"
                        disabled={identity.usedBy.length > 0}
                        title={identity.usedBy.length > 0 ? "Machines wear it: give them another first" : undefined}
                        onClick={() => setDeleting(identity)}
                        leadingIcon={<Icon name="trash" size={12} />}
                        className="hover:text-danger"
                      >
                        Delete
                      </Button>
                    </div>
                    <div className="flex flex-col gap-1 pl-[26px]">
                      <span className="text-2xs text-foreground-quaternary">Subject (issuer {identity.issuer})</span>
                      <CopyLine text={identity.subject} />
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </section>

          <section className="flex flex-col gap-3">
            <h2 className="text-[13px] font-medium text-foreground">Make an identity</h2>
            <NewIdentity count={mine} onAdded={identities.reload} />
          </section>
        </div>
      </div>
      {deleting ? <DeleteIdentity identity={deleting} onClose={() => setDeleting(null)} onDeleted={identities.reload} /> : null}
    </>
  );
};
