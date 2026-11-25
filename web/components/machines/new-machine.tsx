"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";

import { FormField } from "@/components/form/form-field";
import { SelectField } from "@/components/form/select-field";
import { PageHeader } from "@/components/layout/page-header";
import { Button } from "@/components/ui/button";
import { ErrorText } from "@/components/ui/error-text";
import { Icon } from "@/components/ui/icon";
import { TextInput } from "@/components/ui/text-input";
import { loadCatalog } from "@/lib/api/catalog";
import { loadIdentities } from "@/lib/api/identities";
import { loadKeys } from "@/lib/api/keys";
import { get, post } from "@/lib/api/http";
import { formatBytes, kindLabel, profileLabel, profileLabels } from "@/lib/api/images";
import type { Catalog, Machine, NewMachineRequest, VmImage } from "@/lib/api/types";
import { messageOf } from "@/lib/errors";
import { formatCpu, formatMemory } from "@/lib/formatting/format";
import { useAction } from "@/lib/hooks/use-action";
import { useResource } from "@/lib/hooks/use-resource";
import { machinePath } from "@/lib/paths";

import { useSession } from "@/components/session/session-context";

import { useMachines } from "./machines-context";
import { UsageSummary, usageOf } from "./usage";

const passwordMinimum = 8;
const passwordMaximum = 128;
const nameMaximum = 63;

/** A limit that may be missing or null: none. */
const capped = (limit: number | null | undefined): limit is number => typeof limit === "number";

/** Why the chosen machine doesn't fit within the limits, or null when it does. */
const overLimit = (catalog: Catalog, machines: readonly Machine[], cpu: number, memoryMiB: number, diskGiB: number): string | null => {
  const used = usageOf(machines);
  const { limits } = catalog;
  const reasons: string[] = [];
  if (capped(limits.vms) && used.vms + 1 > limits.vms) {
    reasons.push(`you already have ${used.vms} of ${limits.vms} machines`);
  }
  if (capped(limits.cpu) && used.cpu + cpu > limits.cpu) {
    reasons.push(`it needs ${cpu} vCPU and ${Math.max(0, limits.cpu - used.cpu)} are left`);
  }
  if (capped(limits.memoryMiB) && used.memoryMiB + memoryMiB > limits.memoryMiB) {
    reasons.push(
      `it needs ${formatMemory(memoryMiB)} of memory and ${formatMemory(Math.max(0, limits.memoryMiB - used.memoryMiB))} is left`,
    );
  }
  if (capped(limits.diskGiB) && used.diskGiB + diskGiB > limits.diskGiB) {
    reasons.push(`it needs ${diskGiB} GiB of disk and ${Math.max(0, limits.diskGiB - used.diskGiB)} GiB is left`);
  }
  return reasons.length > 0 ? `Over your limits: ${reasons.join("; ")}. Delete a machine or pick a smaller size.` : null;
};

const gibibyte = 1024 ** 3;

/** A machine's name is its host name: a DNS label, unique across everyone (docs/api.md). */
const hostName = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const machineId = /^vm-[a-z0-9]{6}$/;

/** Why a name can't be used, judged locally; null when it looks fine (the server has the last word). */
const nameProblem = (name: string): string | null => {
  if (name === "") {
    return null;
  }
  if (name.length > 63) {
    return "At most 63 characters.";
  }
  if (!hostName.test(name)) {
    return "Lowercase letters, digits and -, not starting or ending with -.";
  }
  if (machineId.test(name)) {
    return "Names like vm-xxxxxx are kept for machine ids.";
  }
  return null;
};

type Availability = { readonly name: string; readonly available: boolean | null; readonly reason: string };

/** Asks the server whether name is free, a moment after typing stops (one request per pause, no polling). */
const useAvailability = (name: string, check: boolean): Availability | null => {
  const [result, setResult] = useState<Availability | null>(null);
  useEffect(() => {
    if (!check) {
      return;
    }
    let cancelled = false;
    const pause = window.setTimeout(() => {
      void get<{ available: boolean; reason?: string }>(`/api/names/${encodeURIComponent(name)}`).then(
        (answer) => {
          if (!cancelled) {
            setResult({ name, available: answer.available, reason: answer.reason ?? "" });
          }
        },
        (failure: unknown) => {
          if (!cancelled) {
            // Couldn't ask: don't block; creating says so if the name is taken.
            setResult({ name, available: null, reason: messageOf(failure) });
          }
        },
      );
    }, 300);
    return () => {
      cancelled = true;
      window.clearTimeout(pause);
    };
  }, [name, check]);
  return result?.name === name ? result : null;
};

/** The smallest root disk an image needs: a disk image's virtual size, rounded up to whole GiB. */
const minimumDiskFor = (image: VmImage | undefined): number =>
  image?.kind === "disk" && image.virtualBytes > 0 ? Math.ceil(image.virtualBytes / gibibyte) : 0;

const imageDescription = (image: VmImage): string =>
  [
    image.mine ? null : `by ${image.owner}`,
    kindLabel(image.kind),
    profileLabel(image.profile),
    formatBytes(image.kind === "disk" && image.virtualBytes > 0 ? image.virtualBytes : image.sizeBytes),
    Object.keys(image.cloudConfig ?? {}).length > 0 ? "preset" : null,
  ]
    .filter(Boolean)
    .join(" · ");

const Form = ({ catalog }: { readonly catalog: Catalog }) => {
  const router = useRouter();
  const { user } = useSession();
  const { machines, put, images } = useMachines();
  const action = useAction();
  const [name, setName] = useState("");
  // Your images first, then everyone's public ones: only those ready to make machines from.
  const readyImages = (images ?? [])
    .filter((image) => image.state === "Ready" && image.kind !== "")
    .sort((a, b) => Number(b.mine) - Number(a.mine));
  const [imageId, setImageId] = useState("");
  const chosenImage = readyImages.find((image) => image.id === imageId) ?? readyImages[0];
  const [profileOverride, setProfileOverride] = useState<string | null>(null);
  const profile = profileOverride ?? chosenImage?.profile ?? "";
  // A Windows install sets its own account and password; there's no cloud-init to take one (or keys).
  const installerSignsIn = chosenImage?.kind === "iso" && profile === "windows";
  const [size, setSize] = useState(catalog.sizes[0]?.id ?? "");
  const [disk, setDisk] = useState(String(catalog.disk.defaultGiB));
  const [password, setPassword] = useState("");
  // "": a new identity, named after the machine; else one of the member's by name.
  const [identity, setIdentity] = useState("");
  const identities = useResource(loadIdentities);
  const ownIdentities = identities.data?.filter((item) => item.mine) ?? [];
  const nameIsAnIdentity = identity === "" && ownIdentities.some((item) => item.name === name.trim());
  const [confirmation, setConfirmation] = useState("");

  const chosenSize = catalog.sizes.find((item) => item.id === size);
  const diskGiB = Number(disk);
  const diskMinimum = Math.max(catalog.disk.minGiB, minimumDiskFor(chosenImage));
  const diskValid = Number.isInteger(diskGiB) && diskGiB >= diskMinimum && diskGiB <= catalog.disk.maxGiB;
  const trimmedName = name.trim();
  const localNameProblem = nameProblem(trimmedName);
  const availability = useAvailability(trimmedName, trimmedName !== "" && localNameProblem === null);
  const nameValid =
    trimmedName !== "" && localNameProblem === null && availability !== null && availability.available !== false;
  const nameStatus =
    trimmedName === ""
      ? null
      : localNameProblem
        ? { tone: "danger", text: localNameProblem }
        : availability === null
          ? { tone: "muted", text: "Checking…" }
          : availability.available === false
            ? { tone: "danger", text: availability.reason || "That name is taken." }
            : availability.available === null
              ? { tone: "muted", text: `Couldn't check (${availability.reason}); creating will tell.` }
              : { tone: "success", text: `${trimmedName} is available.` };
  // The password is optional when the member has an SSH key: then SSH is the way in, and the
  // console's login prompt can't be passed. Without keys it's the only way in (the server says so too).
  const keys = useResource(loadKeys);
  const hasKeys = (keys.data?.length ?? 0) > 0;
  const passwordLength = [...password].length;
  const passwordValid =
    installerSignsIn ||
    (password === "" && hasKeys) ||
    (passwordLength >= passwordMinimum && passwordLength <= passwordMaximum);
  const matches = installerSignsIn || password === confirmation;
  const limitProblem =
    machines && chosenSize ? overLimit(catalog, machines, chosenSize.cpu, chosenSize.memoryMiB, diskValid ? diskGiB : 0) : null;
  const ready =
    nameValid && chosenImage !== undefined && chosenSize && diskValid && passwordValid && matches && !limitProblem && !nameIsAnIdentity;

  const submit = () => {
    if (!ready || !chosenImage) {
      return;
    }
    const body: NewMachineRequest = {
      name: trimmedName,
      image: chosenImage.id,
      ...(profileOverride ? { profile: profileOverride } : {}),
      size,
      diskGiB,
      password: installerSignsIn ? "" : password,
      ...(identity ? { identity } : {}),
    };
    const result: { machine: Machine | null } = { machine: null };
    void action
      .run(async () => {
        result.machine = await post<Machine>("/api/vms", body);
      })
      .then((done) => {
        if (done && result.machine) {
          put(result.machine);
          router.push(machinePath(result.machine.id));
        }
      });
  };

  return (
    <form
      className="flex flex-col gap-5"
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
    >
      {machines ? <UsageSummary catalog={catalog} machines={machines} /> : null}
      <section className="flex flex-col gap-4 rounded-lg border border-border bg-surface p-4">
        <FormField
          label="Name"
          htmlFor="machine-name"
          hint="Its host name, and what you SSH to: lowercase letters, digits and -, up to 63, unique across all members."
        >
          <TextInput
            id="machine-name"
            data-autofocus=""
            value={name}
            maxLength={nameMaximum}
            onChange={(event) => setName(event.target.value)}
            placeholder="my-box"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-invalid={nameStatus?.tone === "danger" || undefined}
          />
        </FormField>
        {nameStatus ? (
          <p
            className={
              nameStatus.tone === "danger" ? "-mt-2 text-xs text-danger" : nameStatus.tone === "success" ? "-mt-2 text-xs text-success" : "-mt-2 text-xs text-foreground-tertiary"
            }
          >
            {nameStatus.text}
          </p>
        ) : null}
        {readyImages.length === 0 ? (
          <p className="rounded-md border border-border bg-background-secondary px-3 py-3 text-xs text-foreground-tertiary">
            {images === null ? "Loading images…" : "No image is ready yet."} Upload an ISO or a disk image, or import one
            from a URL, on the{" "}
            <Link href="/images/" className="text-foreground-secondary underline-offset-2 hover:underline">
              Images
            </Link>{" "}
            page.
          </p>
        ) : (
          <>
            <FormField label="Image">
              <SelectField
                label="Image"
                searchable={readyImages.length > 8}
                items={readyImages.map((image) => ({
                  value: image.id,
                  label: image.mine ? image.name : `${image.name} (${image.owner})`,
                  description: imageDescription(image),
                }))}
                value={chosenImage?.id ?? ""}
                onChange={(next) => {
                  setImageId(next);
                  setProfileOverride(null);
                }}
              />
            </FormField>
            <FormField
              label="OS profile"
              hint={
                chosenImage?.profile
                  ? `Detected: ${profileLabel(chosenImage.profile)}. Change it only if the detection got it wrong.`
                  : "Not detected; pick the one that fits."
              }
            >
              <SelectField
                label="OS profile"
                items={Object.entries(profileLabels).map(([value, label]) => ({
                  value,
                  label,
                  description:
                    value === "windows"
                      ? "UEFI, Secure Boot, TPM, Hyper-V; virtio driver CD"
                      : value === "linux"
                        ? "UEFI, virtio; cloud-init disk attached"
                        : value === "uefi"
                          ? "UEFI firmware, virtio devices"
                          : "Legacy BIOS firmware, virtio devices",
                }))}
                value={profile || "uefi"}
                onChange={setProfileOverride}
              />
            </FormField>
            {chosenImage?.kind === "iso" ? (
              <p className="text-xs text-foreground-tertiary">
                The machine gets a blank disk of the size below and the ISO as a CD-ROM to install from, over the VNC
                console. Eject the install media when you&apos;re done.
              </p>
            ) : (
              <p className="text-xs text-foreground-tertiary">
                The disk image is copied into the machine&apos;s disk and booted. Your account, keys and password apply if
                the image runs cloud-init.
              </p>
            )}
          </>
        )}
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
        <FormField
          label="Disk (GiB)"
          htmlFor="machine-disk"
          hint={`Between ${diskMinimum} and ${catalog.disk.maxGiB} GiB${
            diskMinimum > catalog.disk.minGiB ? " (the disk image needs at least that much)" : ""
          }.`}
        >
          <TextInput
            id="machine-disk"
            type="number"
            inputMode="numeric"
            min={diskMinimum}
            max={catalog.disk.maxGiB}
            step={1}
            value={disk}
            onChange={(event) => setDisk(event.target.value)}
            aria-invalid={!diskValid || undefined}
          />
        </FormField>
        <FormField
          label="Identity"
          hint={
            nameIsAnIdentity
              ? `You already have an identity named ${name.trim()}: choose it here, or name the machine differently.`
              : "What the machine is to other services (its tokens at 169.254.169.254). Machines that wear the same identity share what's granted to it."
          }
        >
          <SelectField
            label="Identity"
            items={[
              { value: "", label: "Its own", description: trimmedName ? `A new identity named ${trimmedName}` : "A new identity named after the machine" },
              ...ownIdentities.map((item) => ({
                value: item.name,
                label: item.name,
                description: item.usedBy.length === 0 ? "Worn by no machine yet" : `Also worn by ${item.usedBy.join(", ")}`,
              })),
            ]}
            value={identity}
            onChange={setIdentity}
          />
        </FormField>
      </section>
      {installerSignsIn ? (
        <section className="flex flex-col gap-1 rounded-lg border border-border bg-surface p-4">
          <h2 className="text-[13px] font-medium text-foreground">Signing in</h2>
          <p className="text-xs text-foreground-tertiary">
            Windows Setup asks for the account and password while it installs; there&apos;s nothing to set here, and your
            SSH keys aren&apos;t added. Install over the VNC console on the machine&apos;s page.
          </p>
        </section>
      ) : (
        <section className="flex flex-col gap-4 rounded-lg border border-border bg-surface p-4">
          <div className="flex flex-col gap-1">
            <h2 className="text-[13px] font-medium text-foreground">Signing in</h2>
            <p className="text-xs text-foreground-tertiary">
              The machine gets an account named <span className="font-mono text-foreground-secondary">{user.username}</span>{" "}
              with sudo
              {hasKeys
                ? `, your ${keys.data?.length === 1 ? "SSH key" : `${keys.data?.length} SSH keys`} (for SSH through the jump host)`
                : ""}
              {" "}and the password below, if the image runs cloud-init
              {chosenImage?.kind === "iso" ? " (installers mostly don't: the installer asks for an account instead)" : ""}.
            </p>
            {hasKeys ? (
              <p className="text-xs text-foreground-tertiary">
                A password is optional: set one only if you want to sign in at the browser console too (e.g. if you break
                the machine&apos;s network).
              </p>
            ) : (
              <p className="text-xs text-foreground-tertiary">
                You have no{" "}
                <Link href="/keys/" className="text-foreground-secondary underline-offset-2 hover:underline">
                  SSH keys
                </Link>{" "}
                yet, so the password is the only way in, at the machine&apos;s console. Add a key to use SSH instead.
              </p>
            )}
            <p className="text-xs text-foreground-tertiary">A password is stored only as a hash and can&apos;t be shown again.</p>
          </div>
          <FormField
            label={hasKeys ? "Password (optional)" : "Password"}
            htmlFor="machine-password"
            hint={`${passwordMinimum}–${passwordMaximum} characters${hasKeys ? ", or leave empty" : ""}.`}
          >
            <TextInput
              id="machine-password"
              type="password"
              value={password}
              maxLength={passwordMaximum * 2}
              onChange={(event) => setPassword(event.target.value)}
              autoComplete="new-password"
            />
          </FormField>
          <FormField label="Repeat password" htmlFor="machine-password-again">
            <TextInput
              id="machine-password-again"
              type="password"
              value={confirmation}
              maxLength={passwordMaximum * 2}
              onChange={(event) => setConfirmation(event.target.value)}
              autoComplete="new-password"
              aria-invalid={(confirmation.length > 0 && !matches) || undefined}
            />
          </FormField>
          {confirmation.length > 0 && !matches ? <p className="text-xs text-danger">The passwords don&apos;t match.</p> : null}
        </section>
      )}
      <ErrorText message={limitProblem} />
      <ErrorText message={action.error} />
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={() => router.push("/")}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" pending={action.pending} disabled={!ready}>
          Create machine
        </Button>
      </div>
    </form>
  );
};

export const NewMachine = () => {
  const catalog = useResource(loadCatalog);
  return (
    <>
      <PageHeader
        title="New machine"
        icon={<Icon name="plus" size={15} />}
        breadcrumbs={["Machines"]}
      />
      <div className="scrollbar-thin min-h-0 min-w-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex max-w-[760px] flex-col gap-5 px-4 py-5 md:px-8 md:py-8">
          <ErrorText message={catalog.error} />
          {catalog.data ? (
            catalog.data.sizes.length === 0 ? (
              <ErrorText message="The catalog has no sizes; ask the administrator." />
            ) : (
              <Form catalog={catalog.data} />
            )
          ) : catalog.error ? null : (
            <div className="py-12 text-center text-xs text-foreground-tertiary">Loading…</div>
          )}
        </div>
      </div>
    </>
  );
};
