"use client";

import { useMachines } from "@/components/machines/machines-context";
import { useSession } from "@/components/session/session-context";
import { Icon } from "@/components/ui/icon";
import { LetterTile } from "@/components/ui/letter-tile";
import { classNames } from "@/lib/utilities/class-names";

import { SidebarLink } from "./sidebar-link";
import { useSidebar } from "./sidebar-provider";
import { UserMenu } from "./user-menu";

export const Sidebar = () => {
  const { configuration, user } = useSession();
  const { machines } = useMachines();
  const { mobileOpen, setMobileOpen } = useSidebar();

  return (
    <>
      {mobileOpen ? (
        <button
          type="button"
          aria-label="Close navigation"
          onClick={() => setMobileOpen(false)}
          className="fixed inset-0 z-40 bg-overlay md:hidden"
        />
      ) : null}
      <aside
        className={classNames(
          "fixed inset-y-0 left-0 z-50 flex w-[280px] max-w-[85vw] shrink-0 flex-col border-r border-border bg-sidebar transition-transform md:static md:z-auto md:w-[232px] md:max-w-none md:translate-x-0",
          mobileOpen ? "translate-x-0 shadow-dialog" : "invisible -translate-x-full md:visible md:shadow-none",
        )}
      >
        <div className="flex h-12 items-center gap-1.5 px-4 text-[13px] font-medium text-foreground md:h-11">
          <LetterTile letter={configuration.title} size="small" className="rounded" />
          <span className="truncate">{configuration.title}</span>
        </div>

        <nav aria-label="Sections" className="scrollbar-thin flex-1 overflow-y-auto px-2 pb-4">
          <div className="flex flex-col gap-px">
            <SidebarLink
              href="/"
              matchPrefixes={["/machine"]}
              icon={<Icon name="server" size={15} />}
              badge={
                machines ? (
                  <span className="text-2xs tabular-nums text-foreground-quaternary">{machines.length}</span>
                ) : null
              }
            >
              Machines
            </SidebarLink>
            <SidebarLink href="/new" icon={<Icon name="plus" size={15} />}>
              New machine
            </SidebarLink>
            <SidebarLink href="/images" icon={<Icon name="hard-drive" size={15} />}>
              Images
            </SidebarLink>
            <SidebarLink href="/identities" icon={<Icon name="shield" size={15} />}>
              Identities
            </SidebarLink>
            <SidebarLink href="/keys" icon={<Icon name="user" size={15} />}>
              Account
            </SidebarLink>
          </div>
          {user.admin ? (
            <div className="mt-4 flex flex-col gap-px">
              <div className="px-2 pb-1 text-2xs font-medium uppercase tracking-wide text-foreground-quaternary">Admin</div>
              <SidebarLink href="/admin/machines" icon={<Icon name="layers" size={15} />}>
                All machines
              </SidebarLink>
              <SidebarLink href="/admin/members" icon={<Icon name="users" size={15} />}>
                Members
              </SidebarLink>
            </div>
          ) : null}
        </nav>

        <div className="flex h-12 items-center gap-1 border-t border-border px-2 md:h-11">
          <UserMenu />
          <span className="flex-1" />
          <button
            type="button"
            onClick={() => setMobileOpen(false)}
            aria-label="Close navigation"
            className="flex h-8 w-8 items-center justify-center rounded-md text-foreground-tertiary hover:bg-background-tertiary hover:text-foreground md:hidden"
          >
            <Icon name="close" size={16} />
          </button>
        </div>
      </aside>
    </>
  );
};
