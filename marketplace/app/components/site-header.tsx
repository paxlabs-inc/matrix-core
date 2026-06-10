import { Link, useLocation, useNavigate } from "react-router";
import { LayoutDashboard, Search } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import AnimatedTabs from "../../components/ui/smoothui/animated-tabs";
import { WalletButton } from "@/components/wallet";
import type { AppUser } from "@/lib/auth.server";

const NAV = [
  { id: "/discover", label: "Discover" },
  { id: "/catalog", label: "Catalog" },
];

export function SiteHeader({
  user,
  wallet,
  allowDev,
}: {
  user: AppUser | null;
  wallet: string | null;
  allowDev: boolean;
}) {
  const location = useLocation();
  const navigate = useNavigate();
  const active = NAV.find((n) => location.pathname.startsWith(n.id))?.id ?? "";

  return (
    <header className="sticky top-0 z-40 w-full">
      <div className="glass border-b border-border/60">
        <div className="mx-auto flex h-16 max-w-7xl items-center gap-6 px-6 sm:px-8">
          <Link to="/" className="flex items-center gap-3">
            <span className="flex size-7 items-center justify-center rounded-lg bg-primary text-primary-foreground">
              <span className="font-display text-base leading-none">D</span>
            </span>
            <span className="font-display text-xl tracking-tight text-foreground">Deus</span>
          </Link>

          <nav className="hidden md:block">
            <AnimatedTabs
              tabs={NAV}
              activeTab={active}
              variant="pill"
              layoutId="site-nav"
              onChange={(id) => navigate(id)}
            />
          </nav>

          <div className="ml-auto flex items-center gap-3">
            <Link
              to="/discover"
              aria-label="Search"
              className="flex size-9 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground md:hidden"
            >
              <Search className="size-4" />
            </Link>

            <WalletButton wallet={wallet} allowDev={allowDev} size="sm" className="hidden sm:inline-flex" />

            {user ? (
              <Link to="/dashboard">
                <SmoothButton variant="secondary" size="sm">
                  <LayoutDashboard className="size-4" />
                  <span className="hidden sm:inline">Dashboard</span>
                </SmoothButton>
              </Link>
            ) : (
              <Link to="/login">
                <SmoothButton variant="secondary" size="sm">
                  Sign in
                </SmoothButton>
              </Link>
            )}
          </div>
        </div>
      </div>
    </header>
  );
}
