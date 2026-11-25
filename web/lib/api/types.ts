/** The shapes of vmservice's API (docs/api.md). */

export type MachineState =
  | "Provisioning"
  | "Starting"
  | "Running"
  | "Stopping"
  | "Stopped"
  | "Error"
  | "Deleting";

export type Machine = {
  readonly id: string;
  readonly name: string;
  readonly image: string;
  readonly imageName: string;
  /** The account to sign in as (the owner's username on cloud-init machines); "" for Windows. */
  readonly user?: string;
  /** Kept for older servers: the same as user. */
  readonly defaultUser: string;
  /** The owner's username: set on other members' machines an admin sees. */
  readonly owner?: string;
  readonly size: string;
  readonly cpu: number;
  readonly memoryMiB: number;
  readonly diskGiB: number;
  readonly state: MachineState;
  readonly detail: string;
  readonly createdAt: string;
  /** The image it was made from (md-…), else absent or "". */
  readonly media?: string;
  /** The OS profile it got ("windows", "linux", "uefi", "bios"). */
  readonly profile?: string;
  /** The install CD-ROMs (ISO, drivers) are still attached; "eject" removes them. */
  readonly installMedia?: boolean;
  /** The name of the identity it wears (what it is to other services). */
  readonly identity?: string;
  readonly identityAccount?: string;
};

/** What machines are to other services: a service account machines wear (several may wear one). */
export type Identity = {
  readonly name: string;
  readonly account: string;
  readonly subject: string;
  readonly issuer: string;
  /** The owner's username. */
  readonly owner: string;
  readonly mine: boolean;
  /** Names of the machines that wear it. */
  readonly usedBy: readonly string[];
  readonly createdAt: string;
};

export type CatalogSize = { readonly id: string; readonly name: string; readonly cpu: number; readonly memoryMiB: number };

export type Resources = {
  readonly vms: number;
  readonly cpu: number;
  readonly memoryMiB: number;
  readonly diskGiB: number;
};

/** A quota: null fields have no limit (admins) or, when editing, mean "the default". */
export type Quota = {
  readonly vms: number | null;
  readonly cpu: number | null;
  readonly memoryMiB: number | null;
  readonly diskGiB: number | null;
};

export type Catalog = {
  readonly sizes: readonly CatalogSize[];
  readonly disk: { readonly minGiB: number; readonly maxGiB: number; readonly defaultGiB: number };
  /** The caller's effective quota; a null (or missing) field: no limit. */
  readonly limits: Quota;
};

export type SessionInfo = { readonly username: string; readonly expires: string; readonly admin?: boolean };

export type NewMachineRequest = {
  readonly name: string;
  /** An image id (md-…). */
  readonly image: string;
  /** Overrides the image's detected profile. */
  readonly profile?: string;
  readonly size: string;
  readonly diskGiB: number;
  readonly password: string;
  /** One of the member's identities by name; absent or "": a new one, named after the machine. */
  readonly identity?: string;
};

export type ImageKind = "iso" | "disk" | "";
export type ImageProfile = "windows" | "linux" | "uefi" | "bios" | "";
export type ImageState = "Preparing" | "Uploading" | "Importing" | "Inspecting" | "Ready" | "Failed";

/** An ISO or disk image a member brought; public ones every member may use (GET /api/images). */
export type VmImage = {
  readonly id: string;
  readonly name: string;
  /** The owner's username. */
  readonly owner: string;
  /** The caller owns it. */
  readonly mine: boolean;
  readonly public: boolean;
  readonly source: "upload" | "url" | "shared";
  readonly kind: ImageKind;
  readonly profile: ImageProfile;
  readonly sizeBytes: number;
  readonly virtualBytes: number;
  readonly state: ImageState;
  readonly progress: string;
  readonly detail: string;
  /** Extras merged into machines' #cloud-config (a "preset"); {} if none. */
  readonly cloudConfig: Readonly<Record<string, unknown>>;
  readonly createdAt: string;
};

export type ImageChange = {
  readonly name?: string;
  readonly public?: boolean;
  readonly cloudConfig?: Readonly<Record<string, unknown>>;
};

/** The caller's account on machines (GET /api/account). */
export type Account = { readonly username: string; readonly uid: number | null };

/** A member as an admin sees it (GET /api/admin/members). */
export type AdminMember = {
  readonly owner: string;
  readonly username: string;
  readonly usage: Resources;
  readonly quota: Quota;
  readonly signedInAt: string;
};

export type NewImageRequest =
  | { readonly name: string; readonly source: "upload"; readonly sizeBytes: number; readonly header: string }
  | { readonly name: string; readonly source: "url"; readonly url: string };

/** A member's SSH public key (GET /api/keys). */
export type SshKey = {
  readonly id: string;
  readonly fingerprint: string;
  readonly type: string;
  readonly label: string;
  readonly createdAt: string;
};

export type NewKeyRequest = { readonly publicKey: string; readonly label: string };
