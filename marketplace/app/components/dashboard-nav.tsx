import { useState } from "react";
import { Form, Link, NavLink, useLocation } from "react-router";
import {
  Coins,
  LayoutGrid,
  type LucideIcon,
  LogOut,
  Menu,
  Plus,
  UserRound,
} from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import Drawer from "../../components/ui/smoothui/drawer";
import { cn } from "@/lib/utils";
import type { AppUser } from "@/lib/auth.server";
import { WalletButton } from "@/components/wallet";

interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { to: "/dashboard", label: "Overview", icon: LayoutGrid, end: true },
  { to: "/dashboard/services/new", label: "New listing", icon: Plus },
  { to: "/dashboard/earnings", label: "Earnings", icon: Coins },
  { to: "/dashboard/account", label: "Account", icon: UserRound },
];

function navLinkClass({ isActive }: { isActive: boolean }) {
  return cn(
    "flex items-center gap-3 rounded-lg px-3 py-3 text-sm font-medium transition-colors",
    isActive
      ? "bg-secondary text-foreground shadow-01"
      : "text-muted-foreground hover:bg-secondary/60 hover:text-foreground"
  );
}

function Wordmark() {
  return (
    <Link to="/dashboard" className="flex items-center gap-3">
      <span className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground shadow-02">
        <span className="font-display text-lg leading-none">D</span>
      </span>
      <span className="font-display text-xl tracking-tight text-foreground">Deus</span>
    </Link>
  );
}

function NavLinks({ onNavigate }: { onNavigate?: () => void }) {
  return (
    <nav className="flex flex-col gap-1">
      {NAV_ITEMS.map((item) => (
        <NavLink key={item.to} to={item.to} end={item.end} className={navLinkClass} onClick={onNavigate}>
          <item.icon className="size-4 shrink-0" aria-hidden />
          {item.label}
        </NavLink>
      ))}
    </nav>
  );
}

function UserPanel({
  user,
  wallet,
  allowDev,
}: {
  user: AppUser;
  wallet: string | null;
  allowDev: boolean;
}) {
  const name = user.displayName || user.email || "Developer";
  const initial = name.charAt(0).toUpperCase();
  return (
    <div className="flex flex-col gap-3 rounded-xl bg-secondary/60 p-3 shadow-01">
      <div className="flex items-center gap-3">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-full bg-primary/15 text-sm font-medium text-[color:var(--accent-fore)]">
          {initial}
        </span>
        <div className="min-w-0">
          <p className="truncate text-sm font-medium text-foreground">{name}</p>
          {user.email ? (
            <p className="truncate text-xs text-muted-foreground">{user.email}</p>
          ) : null}
        </div>
      </div>
      <WalletButton wallet={wallet} allowDev={allowDev} size="sm" className="w-full justify-center" />
      <Form method="post" action="/logout">
        <SmoothButton type="submit" variant="ghost" size="sm" className="w-full justify-start">
          <LogOut className="size-4 shrink-0" aria-hidden />
          Sign out
        </SmoothButton>
      </Form>
    </div>
  );
}

export function DashboardNav({
  user,
  wallet,
  allowDev,
}: {
  user: AppUser;
  wallet: string | null;
  allowDev: boolean;
}) {
  const [open, setOpen] = useState(false);
  const location = useLocation();

  return (
    <>
      {/* Desktop sidebarbg-card, depth via tone + shadow, no border. */}
      <aside className="sticky top-0 hidden h-screen w-64 shrink-0 flex-col gap-6 bg-card px-4 py-6 shadow-03 lg:flex">
        <div className="px-2">
          <Wordmark />
          <p className="eyebrow mt-4 px-1">Developer</p>
        </div>
        <div className="flex-1">
          <NavLinks />
        </div>
        <UserPanel user={user} wallet={wallet} allowDev={allowDev} />
      </aside>

      {/* Mobile top bar + smoothui drawer */}
      <div className="sticky top-0 z-30 flex items-center justify-between bg-card px-4 py-3 shadow-02 lg:hidden">
        <Wordmark />
        <Drawer
          key={location.pathname}
          open={open}
          onOpenChange={setOpen}
          side="left"
          title="Deus"
          description="Developer dashboard"
          trigger={
            <SmoothButton type="button" variant="secondary" size="icon" aria-label="Open menu">
              <Menu className="size-5" />
            </SmoothButton>
          }
        >
          <div className="flex flex-col gap-6 pt-2">
            <NavLinks onNavigate={() => setOpen(false)} />
            <UserPanel user={user} wallet={wallet} allowDev={allowDev} />
          </div>
        </Drawer>
      </div>
    </>
  );
}

export default DashboardNav;
