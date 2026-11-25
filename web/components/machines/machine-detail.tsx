"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useEffect, useState } from "react";

import { Details } from "@/components/data/details";
import { FormField } from "@/components/form/form-field";
import { SelectField } from "@/components/form/select-field";
import { PageHeader } from "@/components/layout/page-header";
import { useSession } from "@/components/session/session-context";
import { Button } from "@/components/ui/button";
import { CopyLine } from "@/components/ui/copy-line";
import { DialogFrame } from "@/components/ui/dialog-frame";
import { TextInput } from "@/components/ui/text-input";
import { EmptyState } from "@/components/ui/empty-state";
import { ErrorText } from "@/components/ui/error-text";
import { Icon } from "@/components/ui/icon";
import { loadCatalog } from "@/lib/api/catalog";
import { loadIdentities } from "@/lib/api/identities";
import { ApiError, get, post, put, remove } from "@/lib/api/http";
import type { Catalog, Machine } from "@/lib/api/types";
import { formatCpu, formatMemory, formatTime } from "@/lib/formatting/format";
import { profileLabel } from "@/lib/api/images";
import { useAction } from "@/lib/hooks/use-action";
import { useResource } from "@/lib/hooks/use-resource";
import { messageOf } from "@/lib/errors";
import { sshCommand } from "@/lib/paths";
import { classNames } from "@/lib/utilities/class-names";

import { LiveNotice } from "./live-notice";
import { MachineStateBadge } from "./machine-state";
import { useMachines } from "./machines-context";
import { ConsoleTab } from "./console-tab";

const Section = ({ title, children }: { readonly title: string; readonly children: React.ReactNode }) => (
  <section className="flex flex-col gap-3">
    <h2 className="text-[13px] font-medium text-foreground">{title}</h2>
    {children}
  </section>
);

const DeleteDialog = ({ machine, onClose }: { readonly machine: Machine; readonly onClose: () => void }) => {
  const router = useRouter();
  const action = useAction();
  const [typed, setTyped] = useState("");
  const [withIdentity, setWithIdentity] = useState(false);
  // The member types the machine's name to confirm: deleting can't be undone.
  const confirmed = typed === machine.name;
  return (
    <DialogFrame
      title={`Delete ${machine.name}?`}
      onClose={onClose}
      submitLabel="Delete machine"
      danger
      submitDisabled={!confirmed}
      pending={action.pending}
      error={action.error}
      onSubmit={() => {
        if (!confirmed) {
          return;
        }
        const query = withIdentity ? "?identity=delete" : "";
        void action.run(() => remove(`/api/vms/${encodeURIComponent(machine.id)}${query}`)).then((done) => {
          if (done) {
            router.push("/");
          }
        });
      }}
    >
      <p className="text-[13px] text-foreground-secondary">
        The machine and its {machine.diskGiB} GiB disk are deleted, with everything on it. This can&apos;t be undone.
      </p>
      <label className="mt-3 flex flex-col gap-1.5 text-[13px] text-foreground-secondary" htmlFor="delete-confirm">
        <span>
          Type <span className="font-mono text-foreground">{machine.name}</span> to confirm.
        </span>
        <TextInput
          id="delete-confirm"
          data-autofocus=""
          value={typed}
          onChange={(event) => setTyped(event.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
      </label>
      {machine.identity ? (
        <label className="mt-3 flex items-start gap-2 text-[13px] text-foreground-secondary">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={withIdentity}
            onChange={(event) => setWithIdentity(event.target.checked)}
          />
          <span>
            Delete its identity <span className="font-mono text-foreground">{machine.identity}</span> too, if no other machine
            wears it then. What was granted to it stops working.
          </span>
        </label>
      ) : null}
    </DialogFrame>
  );
};

const IdentityDialog = ({ machine, onClose }: { readonly machine: Machine; readonly onClose: () => void }) => {
  const action = useAction();
  const identities = useResource(loadIdentities);
  const [chosen, setChosen] = useState(machine.identity ?? "");
  // The owner's identities: a machine wears only its owner's.
  const owners = (identities.data ?? []).filter((item) => (machine.owner ? item.owner === machine.owner : item.mine));
  const live = machine.state === "Running" || machine.state === "Starting";
  return (
    <DialogFrame
      title="Wear another identity?"
      onClose={onClose}
      submitLabel="Change identity"
      pending={action.pending}
      submitDisabled={chosen === "" || chosen === machine.identity}
      error={action.error ?? identities.error}
      onSubmit={() =>
        void action.run(() => put(`/api/vms/${encodeURIComponent(machine.id)}/identity`, { identity: chosen })).then((done) => {
          if (done) {
            onClose();
          }
        })
      }
    >
      <div className="flex flex-col gap-3">
        {identities.data ? (
          <FormField label="Identity">
            <SelectField
              label="Identity"
              items={owners.map((item) => ({
                value: item.name,
                label: item.name,
                description: item.usedBy.length === 0 ? "Worn by no machine" : `Worn by ${item.usedBy.join(", ")}`,
              }))}
              value={chosen}
              onChange={setChosen}
            />
          </FormField>
        ) : null}
        <p className="text-[13px] text-foreground-secondary">
          Make new identities on the Identities page. The one it wears now stays there.{" "}
          {live
            ? "The machine keeps its current identity until you Restart or Stop and Start it here."
            : "It applies the next time the machine starts."}
        </p>
      </div>
    </DialogFrame>
  );
};

const EjectDialog = ({ machine, onClose }: { readonly machine: Machine; readonly onClose: () => void }) => {
  const action = useAction();
  return (
    <DialogFrame
      title="Eject the install media?"
      onClose={onClose}
      submitLabel="Eject"
      pending={action.pending}
      error={action.error}
      onSubmit={() =>
        void action.run(() => post(`/api/vms/${encodeURIComponent(machine.id)}/eject`)).then((done) => {
          if (done) {
            onClose();
          }
        })
      }
    >
      <p className="text-[13px] text-foreground-secondary">
        The ISO{machine.profile === "windows" ? " and the driver CD are" : " is"} removed, so the machine boots from its
        disk.{" "}
        {machine.state === "Running" || machine.state === "Starting"
          ? "The machine keeps running; the CDs go the next time you Restart or Stop and Start it here (a restart inside the machine doesn't count)."
          : "It takes effect the next time the machine starts."}
      </p>
    </DialogFrame>
  );
};

const ResizeDialog = ({ machine, onClose }: { readonly machine: Machine; readonly onClose: () => void }) => {
  const action = useAction();
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [size, setSize] = useState(machine.size);
  useEffect(() => {
    loadCatalog().then(setCatalog, (error: unknown) => setLoadError(messageOf(error)));
  }, []);
  const live = machine.state === "Running" || machine.state === "Starting";
  return (
    <DialogFrame
      title="Change the size?"
      onClose={onClose}
      submitLabel="Change size"
      pending={action.pending}
      submitDisabled={catalog === null || size === machine.size}
      error={action.error ?? loadError}
      onSubmit={() =>
        void action.run(() => put(`/api/vms/${encodeURIComponent(machine.id)}/size`, { size })).then((done) => {
          if (done) {
            onClose();
          }
        })
      }
    >
      <div className="flex flex-col gap-3">
        {catalog ? (
          <FormField label="Size">
            <SelectField
              label="Size"
              items={catalog.sizes.map((item) => ({
                value: item.id,
                label: item.name,
                description: `${formatCpu(item.cpu)} · ${formatMemory(item.memoryMiB)}`,
              }))}
              value={size}
              onChange={setSize}
            />
          </FormField>
        ) : null}
        <p className="text-[13px] text-foreground-secondary">
          The disk stays as it is.{" "}
          {live
            ? "The machine keeps running at its current size; the new size applies the next time you Restart or Stop and Start it here (a restart inside the machine doesn't count)."
            : "It applies the next time the machine starts."}
        </p>
      </div>
    </DialogFrame>
  );
};

const Actions = ({ machine }: { readonly machine: Machine }) => {
  const action = useAction();
  const [deleting, setDeleting] = useState(false);
  const [ejecting, setEjecting] = useState(false);
  const [resizing, setResizing] = useState(false);
  const [changingIdentity, setChangingIdentity] = useState(false);
  const run = (verb: "start" | "stop" | "restart") =>
    void action.run(() => post(`/api/vms/${encodeURIComponent(machine.id)}/${verb}`));
  const { state } = machine;
  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex flex-wrap items-center justify-end gap-1">
        {state === "Stopped" ? (
          <Button pending={action.pending} onClick={() => run("start")} leadingIcon={<Icon name="play" size={12} />}>
            Start
          </Button>
        ) : null}
        {state === "Running" || state === "Starting" || state === "Error" ? (
          <Button pending={action.pending} onClick={() => run("stop")} leadingIcon={<Icon name="square" size={12} />}>
            Stop
          </Button>
        ) : null}
        {state === "Running" ? (
          <Button pending={action.pending} onClick={() => run("restart")} leadingIcon={<Icon name="refresh" size={12} />}>
            Restart
          </Button>
        ) : null}
        {machine.installMedia && state !== "Deleting" ? (
          <Button onClick={() => setEjecting(true)} leadingIcon={<Icon name="arrow-up" size={12} />}>
            Eject install media
          </Button>
        ) : null}
        {state !== "Deleting" ? (
          <Button onClick={() => setResizing(true)} leadingIcon={<Icon name="sliders" size={12} />}>
            Resize
          </Button>
        ) : null}
        {state !== "Deleting" ? (
          <Button onClick={() => setChangingIdentity(true)} leadingIcon={<Icon name="shield" size={12} />}>
            Identity
          </Button>
        ) : null}
        <Button
          variant="ghost"
          disabled={state === "Deleting"}
          onClick={() => setDeleting(true)}
          leadingIcon={<Icon name="trash" size={12} />}
          className="hover:text-danger"
        >
          Delete
        </Button>
      </div>
      <ErrorText message={action.error} />
      {deleting ? <DeleteDialog machine={machine} onClose={() => setDeleting(false)} /> : null}
      {ejecting ? <EjectDialog machine={machine} onClose={() => setEjecting(false)} /> : null}
      {resizing ? <ResizeDialog machine={machine} onClose={() => setResizing(false)} /> : null}
      {changingIdentity ? <IdentityDialog machine={machine} onClose={() => setChangingIdentity(false)} /> : null}
    </div>
  );
};

type Tab = "overview" | "console";

/** The account to sign in as: the owner's username on cloud-init machines. */
const userOf = (machine: Machine): string => machine.user || machine.defaultUser || "";

const Overview = ({ machine }: { readonly machine: Machine }) => {
  const { configuration } = useSession();
  // A machine's name is its host name, unique across everyone: ssh -J … user@<name>.
  const { machines: mine } = useMachines();
  const own = !machine.owner || (mine ?? []).some((other) => other.id === machine.id);
  const user = userOf(machine);
  const windows = machine.profile === "windows";
  return (
    <div className="scrollbar-thin min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex max-w-[1080px] flex-col gap-8 px-4 py-5 md:px-8 md:py-6">
        {windows && machine.installMedia ? (
          <Section title="Installing Windows">
            <ol className="flex list-decimal flex-col gap-1.5 pl-5 text-xs text-foreground-tertiary">
              <li>
                Open the Console tab with VNC <em>before</em> the machine starts (or restart it with VNC open), and press a
                key when it says &ldquo;Press any key to boot from CD or DVD&rdquo;. Otherwise it falls back to the
                firmware menu.
              </li>
              <li>
                When Setup says a media driver is missing, or lists no disk: choose{" "}
                <span className="text-foreground-secondary">Load driver</span>, browse to the virtio CD and pick{" "}
                <span className="font-mono text-foreground-secondary">vioscsi</span> (for your Windows version, amd64).
                That one driver serves both the install CD and the disk; load nothing else.
              </li>
              <li>
                At &ldquo;Let&rsquo;s connect you to a network&rdquo;, choose{" "}
                <span className="text-foreground-secondary">Install drivers</span> and pick{" "}
                <span className="font-mono text-foreground-secondary">NetKVM</span> (for your Windows version, amd64) on
                the virtio CD.
              </li>
              <li>Finish Setup; it sets the account and password.</li>
              <li>
                On the desktop, run <span className="font-mono text-foreground-secondary">virtio-win-guest-tools.exe</span>{" "}
                from the virtio CD for the remaining drivers and the guest agent.
              </li>
              <li>
                Then <span className="text-foreground-secondary">Eject install media</span> (above), so the machine boots
                from its disk.
              </li>
            </ol>
          </Section>
        ) : null}
        <Section title="Details">
          <Details
            items={[
              ...(!own && machine.owner ? ([["Owner", machine.owner]] as [string, React.ReactNode][]) : []),
              ["Image", machine.imageName || machine.media || "—"],
              ...(machine.profile ? ([["OS profile", profileLabel(machine.profile)]] as [string, React.ReactNode][]) : []),
              ["Size", `${formatCpu(machine.cpu)} · ${formatMemory(machine.memoryMiB)}`],
              ["Disk", `${machine.diskGiB} GiB`],
              ...(machine.identity
                ? ([["Identity", <Link key="identity" href="/identities/" className="underline-offset-2 hover:underline">{machine.identity}</Link>]] as [string, React.ReactNode][])
                : []),
              ...(user && !windows
                ? ([["User", <span key="user" className="font-mono text-xs">{user}</span>]] as [string, React.ReactNode][])
                : []),
              ["Created", formatTime(machine.createdAt)],
              ["ID", <span key="id" className="font-mono text-xs">{machine.id}</span>],
            ]}
          />
        </Section>
        <Section title="Connect">
          {windows ? (
            <div className="flex flex-col gap-3 text-xs text-foreground-tertiary">
              <p>Sign in at the VNC console with the account you made during Windows Setup.</p>
              {configuration.ssh && own ? (
                <div className="flex flex-col gap-1.5">
                  <p>
                    For Remote Desktop: enable <span className="text-foreground-secondary">OpenSSH Server</span> in
                    Windows (Settings → System → Optional features), then tunnel RDP through the jump host and connect
                    your RDP client to <span className="font-mono text-foreground-secondary">localhost</span>:
                  </p>
                  <CopyLine
                    text={`${sshCommand(configuration.ssh, "<windows user>", machine.name)} -L 3389:localhost:3389`}
                  />
                </div>
              ) : null}
            </div>
          ) : (
          <div className="flex flex-col gap-3 text-xs text-foreground-tertiary">
            <p>
              On the console, sign in as{" "}
              <span className="font-mono text-foreground-secondary">{user || "the image's account"}</span> with the password
              set when the machine was made, if one was (only if the image runs cloud-init; an installed system uses the
              account made during installation).
            </p>
            {configuration.ssh && own ? (
              <div className="flex flex-col gap-1.5">
                <p>
                  With SSH, through the jump host to the machine&apos;s name; the machine signs you in with a key you
                  registered under{" "}
                  <Link href="/keys/" className="text-foreground-secondary underline-offset-2 hover:underline">
                    Account
                  </Link>{" "}
                  before it was made (or the password):
                </p>
                <CopyLine text={sshCommand(configuration.ssh, user || "<user>", machine.name)} />
              </div>
            ) : null}
          </div>
          )}
        </Section>
      </div>
    </div>
  );
};

/**
 * The machine: from the live list (own machines, and other members' an admin has seen). An admin
 * opening another member's machine directly has it fetched once (GET /api/vms/{id}); its events
 * keep it current from then on.
 */
const useMachine = (id: string): { readonly machine: Machine | null; readonly loading: boolean; readonly error: string | null } => {
  const { user } = useSession();
  const { machines, allMachines, put } = useMachines();
  const known = allMachines.find((item) => item.id === id) ?? null;
  const [fetched, setFetched] = useState<{ readonly id: string; readonly error: string | null } | null>(null);
  const admin = user.admin === true;
  const wanted = admin && machines !== null && !known && id !== "" && fetched?.id !== id;
  useEffect(() => {
    if (!wanted) {
      return;
    }
    let cancelled = false;
    void get<Machine>(`/api/vms/${encodeURIComponent(id)}`).then(
      (machine) => {
        if (!cancelled) {
          put(machine);
          setFetched({ id, error: null });
        }
      },
      (failure: unknown) => {
        if (!cancelled) {
          setFetched({ id, error: failure instanceof ApiError && failure.status === 404 ? null : messageOf(failure) });
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [wanted, id, put]);
  const loading = machines === null || (admin && !known && fetched?.id !== id);
  return { machine: known, loading, error: fetched?.id === id ? fetched.error : null };
};

export const MachineDetail = () => {
  const { user } = useSession();
  const id = useSearchParams().get("id") ?? "";
  const { machine, loading, error } = useMachine(id);
  const [tab, setTab] = useState<Tab>("overview");

  const tabButton = (value: Tab, label: string) => (
    <button
      type="button"
      role="tab"
      aria-selected={tab === value}
      onClick={() => setTab(value)}
      className={classNames(
        "-mb-px h-9 border-b-2 px-1 text-[13px] transition-colors",
        tab === value
          ? "border-foreground font-medium text-foreground"
          : "border-transparent text-foreground-secondary hover:text-foreground",
      )}
    >
      {label}
    </button>
  );

  return (
    <>
      <PageHeader
        title={machine?.name ?? id}
        icon={<Icon name="server" size={15} />}
        breadcrumbs={["Machines"]}
      />
      {loading ? (
        <div className="mx-auto flex w-full max-w-[1080px] flex-col gap-4 px-4 py-5 md:px-8 md:py-8">
          <LiveNotice />
          <div className="py-12 text-center text-xs text-foreground-tertiary">Loading…</div>
        </div>
      ) : error && !machine ? (
        <div className="mx-auto w-full max-w-[1080px] px-4 py-5 md:px-8 md:py-8">
          <ErrorText message={error} />
        </div>
      ) : !machine ? (
        <EmptyState
          fill
          icon={<Icon name="server" size={18} />}
          title="No such machine"
          description="It was deleted, or it isn't yours."
        />
      ) : (
        <div className="flex min-h-0 flex-1 flex-col">
          <div className="flex flex-col gap-3 border-b border-border px-4 pt-4 md:px-8">
            <LiveNotice />
            <div className="flex flex-wrap items-start gap-4">
              <div className="flex min-w-0 flex-1 flex-col gap-1">
                <div className="flex items-center gap-2">
                  <h2 className="text-base font-semibold text-foreground [overflow-wrap:anywhere]">{machine.name}</h2>
                  <MachineStateBadge state={machine.state} />
                </div>
                {machine.owner && machine.owner !== user.username ? (
                  <p className="text-xs text-foreground-tertiary">Owned by {machine.owner}</p>
                ) : null}
                {machine.detail ? (
                  <p className={classNames("text-xs", machine.state === "Error" ? "text-danger" : "text-foreground-tertiary")}>
                    {machine.detail}
                  </p>
                ) : null}
              </div>
              <Actions machine={machine} />
            </div>
            <div role="tablist" aria-label="Machine" className="flex gap-4">
              {tabButton("overview", "Overview")}
              {tabButton("console", "Console")}
            </div>
          </div>
          {tab === "overview" ? <Overview machine={machine} /> : <ConsoleTab machine={machine} />}
        </div>
      )}
    </>
  );
};
