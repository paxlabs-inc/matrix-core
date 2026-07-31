'use client'

/**
 * pax-icons — SVG sprite-based icon system ported from paxscan.
 *
 * Icons are served as a single compiled sprite at /icons/sprite.svg and
 * referenced via <use href="/icons/sprite.svg#name"/>. This is lighter than
 * inlining SVG paths per-icon and allows the browser to cache the sprite across
 * the whole app. The sprite is built from /icons/*.svg by svg-icons-cli.
 *
 * Usage:
 *   import { PaxIcon } from '@/lib/pax-icons'
 *   <PaxIcon name="search" size={20} />
 *
 *   import { SearchIcon, CheckIcon } from '@/lib/pax-icons'
 *   <CheckIcon className="size-4" />
 */
import * as React from 'react'
import type { IconName } from '@/public/icons/name'

export type { IconName }

export const SPRITE_URL = '/icons/sprite.svg'

export interface SpriteIconProps extends React.SVGProps<SVGSVGElement> {
  name: IconName
  /** Width and height in px. Defaults to 24. */
  size?: number | string
}

/**
 * Render a paxscan sprite icon by name.
 *
 * The host <svg> intentionally has no stroke/fill override so polychrome
 * icons keep their original colours, while monochrome icons still inherit
 * `currentColor` from CSS. Set `color` or `className` to tint when needed.
 */
export const PaxIcon = React.forwardRef<SVGSVGElement, SpriteIconProps>(function PaxIcon(
  { name, size = 24, className, ...props },
  ref,
) {
  return (
    <svg ref={ref} width={size} height={size} aria-hidden="true" className={className} {...props}>
      <use href={`${SPRITE_URL}#${name}`} />
    </svg>
  )
})

PaxIcon.displayName = 'PaxIcon'

// ---------------------------------------------------------------------------
// Named exports — one per sprite symbol, matching the IconName union.
// These are thin wrappers so you can use them as drop-in icon components.
// ---------------------------------------------------------------------------

function makePaxIcon(name: IconName) {
  const Icon = React.forwardRef<SVGSVGElement, Omit<SpriteIconProps, 'name'>>(
    function SpriteIconWrapper({ size = 24, ...props }, ref) {
      return <PaxIcon ref={ref} name={name} size={size} {...props} />
    },
  )
  Icon.displayName = name
  return Icon
}

// ── General UI ──────────────────────────────────────────────────────────────
export const SearchIcon = makePaxIcon('search')
export const FilterIcon = makePaxIcon('filter')
export const CloseIcon = makePaxIcon('close')
export const PlusIcon = makePaxIcon('plus')
export const MinusIcon = makePaxIcon('minus')
export const CheckIcon = makePaxIcon('check')
export const EditIcon = makePaxIcon('edit')
export const CopyIcon = makePaxIcon('copy')
export const CopyCheckIcon = makePaxIcon('copy_check')
export const DeleteIcon = makePaxIcon('delete')
export const ShareIcon = makePaxIcon('share')
export const RefreshIcon = makePaxIcon('refresh')
export const GearIcon = makePaxIcon('gear')
export const LinkIcon = makePaxIcon('link')
export const LinkExternalIcon = makePaxIcon('link_external')
export const OpenLinkIcon = makePaxIcon('open-link')
export const LockIcon = makePaxIcon('lock')
export const KeyIcon = makePaxIcon('key')
export const StarFilledIcon = makePaxIcon('star_filled')
export const StarOutlineIcon = makePaxIcon('star_outline')
export const HeartFilledIcon = makePaxIcon('heart_filled')
export const HeartOutlineIcon = makePaxIcon('heart_outline')
export const DotsIcon = makePaxIcon('dots')
export const BurgerIcon = makePaxIcon('burger')
export const ProfileIcon = makePaxIcon('profile')
export const SignOutIcon = makePaxIcon('sign_out')
export const QrCodeIcon = makePaxIcon('qr_code')
export const RepeatIcon = makePaxIcon('repeat')
export const ReturnIcon = makePaxIcon('return')
export const RevokeIcon = makePaxIcon('revoke')
export const SwapIcon = makePaxIcon('swap')
export const MultisendIcon = makePaxIcon('multisend')
export const ColumnsIcon = makePaxIcon('columns')
export const ListViewIcon = makePaxIcon('list_view')
export const AdvancedFilterIcon = makePaxIcon('advanced-filter')

// ── Status ───────────────────────────────────────────────────────────────────
export const StatusSuccessIcon = makePaxIcon('status/success')
export const StatusErrorIcon = makePaxIcon('status/error')
export const StatusPendingIcon = makePaxIcon('status/pending')
export const StatusWarningIcon = makePaxIcon('status/warning')
export const InfoIcon = makePaxIcon('info')
export const InfoFilledIcon = makePaxIcon('info_filled')
export const ScamIcon = makePaxIcon('scam')
export const CrossIcon = makePaxIcon('cross')
export const CertifiedIcon = makePaxIcon('certified')
export const VerifiedIcon = makePaxIcon('verified')

// ── Time / blocks ─────────────────────────────────────────────────────────────
export const ClockIcon = makePaxIcon('clock')
export const ClockLightIcon = makePaxIcon('clock-light')
export const HourglassIcon = makePaxIcon('hourglass')
export const BlockCountdownIcon = makePaxIcon('block_countdown')
export const CheckeredFlagIcon = makePaxIcon('checkered_flag')

// ── Theme ────────────────────────────────────────────────────────────────────
export const SunIcon = makePaxIcon('sun')
export const MoonIcon = makePaxIcon('moon')
export const MoonWithStarIcon = makePaxIcon('moon-with-star')

// ── Blockchain ────────────────────────────────────────────────────────────────
export const BlockIcon = makePaxIcon('block')
export const TransactionsIcon = makePaxIcon('transactions')
export const TxnBatchesIcon = makePaxIcon('txn_batches')
export const UserOpIcon = makePaxIcon('user_op')
export const BlobIcon = makePaxIcon('blob')
export const OperationIcon = makePaxIcon('operation')
export const GasIcon = makePaxIcon('gas')
export const GasXlIcon = makePaxIcon('gas_xl')
export const FlameIcon = makePaxIcon('flame')
export const FlashblockIcon = makePaxIcon('flashblock')
export const ClustersIcon = makePaxIcon('clusters')
export const NetworksIcon = makePaxIcon('networks')
export const InteropIcon = makePaxIcon('interop')
export const HexagonIcon = makePaxIcon('hexagon')
export const ScopeIcon = makePaxIcon('scope')

// ── Tokens / NFTs ─────────────────────────────────────────────────────────────
export const TokensIcon = makePaxIcon('tokens')
export const NftShieldIcon = makePaxIcon('nft_shield')
export const CollectionIcon = makePaxIcon('collection')
export const TokenPlaceholderIcon = makePaxIcon('token-placeholder')

// ── Wallet / payments ─────────────────────────────────────────────────────────
export const WalletIcon = makePaxIcon('wallet')
export const PaymentLinkIcon = makePaxIcon('payment_link')
export const CoinsIcon = makePaxIcon('coins/bitcoin')

// ── Contracts ─────────────────────────────────────────────────────────────────
export const ContractRegularIcon = makePaxIcon('contracts/regular')
export const ContractVerifiedIcon = makePaxIcon('contracts/verified')
export const ContractProxyIcon = makePaxIcon('contracts/proxy')

// ── Globe / explorer ─────────────────────────────────────────────────────────
export const GlobeIcon = makePaxIcon('globe')
export const ExplorerIcon = makePaxIcon('explorer')

// ── Apps / integrations ───────────────────────────────────────────────────────
export const AppsIcon = makePaxIcon('apps')
export const AppsListIcon = makePaxIcon('apps_list')
export const AbiIcon = makePaxIcon('ABI')
export const ApiIcon = makePaxIcon('API')
export const RpcIcon = makePaxIcon('RPC')

// ── Merits / gamification ─────────────────────────────────────────────────────
export const MeritsIcon = makePaxIcon('merits')
export const MeritsColoredIcon = makePaxIcon('merits_colored')
export const MeritsWithDotIcon = makePaxIcon('merits_with_dot')
export const RocketIcon = makePaxIcon('rocket')
export const RocketXlIcon = makePaxIcon('rocket_xl')
export const LightningIcon = makePaxIcon('lightning')
export const PieChartIcon = makePaxIcon('pie_chart')

// ── Social ────────────────────────────────────────────────────────────────────
export const TwitterIcon = makePaxIcon('social/twitter')
export const TwitterFilledIcon = makePaxIcon('social/twitter_filled')
export const GithubFilledIcon = makePaxIcon('social/github_filled')
export const DiscordIcon = makePaxIcon('social/discord')
export const DiscordFilledIcon = makePaxIcon('social/discord_filled')
export const TelegramFilledIcon = makePaxIcon('social/telegram_filled')
export const LinkedinFilledIcon = makePaxIcon('social/linkedin_filled')
export const RedditFilledIcon = makePaxIcon('social/reddit_filled')

// ── ENS / tags ────────────────────────────────────────────────────────────────
export const EnsIcon = makePaxIcon('ENS')
export const PrivateTagsIcon = makePaxIcon('private_tags')
export const PublicTagsIcon = makePaxIcon('publictags')
export const EmailIcon = makePaxIcon('email')

// ── Arrows ────────────────────────────────────────────────────────────────────
export const ArrowUpDownIcon = makePaxIcon('arrows/up-down')
export const ArrowUpHeadIcon = makePaxIcon('arrows/up-head')
export const ArrowEastIcon = makePaxIcon('arrows/east')
export const ArrowDownRightIcon = makePaxIcon('arrows/down-right')

// ── Brands ────────────────────────────────────────────────────────────────────
export const BlockscoutIcon = makePaxIcon('brands/blockscout')
export const UniswapIcon = makePaxIcon('uniswap')
export const MudIcon = makePaxIcon('MUD')

// ── Navigation (named) ────────────────────────────────────────────────────────
export const NavBlockchainIcon = makePaxIcon('navigation/blockchain')
export const NavTokensIcon = makePaxIcon('navigation/tokens')
export const NavTransactionsIcon = makePaxIcon('navigation/transactions')
export const NavContractsIcon = makePaxIcon('navigation/verified_contracts')
export const NavStatsIcon = makePaxIcon('navigation/stats')
export const NavAppsIcon = makePaxIcon('navigation/apps')
export const NavMeritsIcon = makePaxIcon('navigation/merits')
