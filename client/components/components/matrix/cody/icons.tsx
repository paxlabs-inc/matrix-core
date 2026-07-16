// ─── Solid Rounded Icon Library ───
// Filled, rounded style with consistent visual weight
// Warm charcoal + sage palette via currentColor

import React from 'react'

const p = {
  xmlns: 'http://www.w3.org/2000/svg',
  width: '24',
  height: '24',
  viewBox: '0 0 24 24',
  fill: 'currentColor',
}

// ─── NAVIGATION ───

export const IconHome = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M10 20v-6h4v6h5v-8h3L12 3 2 12h3v8z" />
  </svg>
)

export const IconSearch = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M15.5 14h-.79l-.28-.27A6.471 6.471 0 0016 9.5 6.5 6.5 0 109.5 16c1.61 0 3.09-.59 4.23-1.57l.27.28v.79l5 4.99L20.49 19l-4.99-5zm-6 0C7.01 14 5 11.99 5 9.5S7.01 5 9.5 5 14 7.01 14 9.5 11.99 14 9.5 14z" />
  </svg>
)

export const IconMenu = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 18h18v-2H3v2zm0-5h18v-2H3v2zm0-7v2h18V6H3z" />
  </svg>
)

export const IconArrowLeft = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M15.41 7.41L14 6l-6 6 6 6 1.41-1.41L10.83 12z" />
  </svg>
)

export const IconArrowRight = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M10 6L8.59 7.41 13.17 12l-4.58 4.59L10 18l6-6z" />
  </svg>
)

export const IconArrowUp = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M7.41 15.41L12 10.83l4.59 4.58L18 14l-6-6-6 6z" />
  </svg>
)

export const IconArrowDown = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M7.41 8.59L12 13.17l4.59-4.58L18 10l-6 6-6-6z" />
  </svg>
)

export const IconChevronLeft = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M15.41 7.41L14 6l-6 6 6 6 1.41-1.41L10.83 12z" />
  </svg>
)

export const IconChevronRight = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M10 6L8.59 7.41 13.17 12l-4.58 4.59L10 18l6-6z" />
  </svg>
)

export const IconChevronDown = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M7.41 8.59L12 13.17l4.59-4.58L18 10l-6 6-6-6z" />
  </svg>
)

export const IconChevronUp = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M7.41 15.41L12 10.83l4.59 4.58L18 14l-6-6-6 6z" />
  </svg>
)

// ─── ACTIONS ───

export const IconPlus = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z" />
  </svg>
)

export const IconClose = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
  </svg>
)

export const IconCheck = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M9 16.17L4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41z" />
  </svg>
)

export const IconEdit = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zM20.71 7.04a.996.996 0 000-1.41l-2.34-2.34a.996.996 0 00-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z" />
  </svg>
)

export const IconTrash = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6 19c0 1.1.9 2 2 2h8c1.1 0 2-.9 2-2V7H6v12zM19 4h-3.5l-1-1h-5l-1 1H5v2h14V4z" />
  </svg>
)

export const IconCopy = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z" />
  </svg>
)

export const IconRefresh = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M17.65 6.35A7.958 7.958 0 0012 4c-4.42 0-7.99 3.58-7.99 8s3.57 8 7.99 8c3.73 0 6.84-2.55 7.73-6h-2.08A5.99 5.99 0 0112 18c-3.31 0-6-2.69-6-6s2.69-6 6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z" />
  </svg>
)

export const IconDownload = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19 9h-4V3H9v6H5l7 7 7-7zM5 18v2h14v-2H5z" />
  </svg>
)

export const IconUpload = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M9 16h6v-6h4l-7-7-7 7h4zm-4 2h14v2H5v-2z" />
  </svg>
)

export const IconExternal = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19 19H5V5h7V3H5a2 2 0 00-2 2v14a2 2 0 002 2h14c1.1 0 2-.9 2-2v-7h-2v7zM14 3v2h3.59l-9.83 9.83 1.41 1.41L19 6.41V10h2V3h-7z" />
  </svg>
)

export const IconMore = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 8c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zm0 2c-1.1 0-2 .9-2 2s.9 2 2 2 2-.9 2-2-.9-2-2-2zm0 6c-1.1 0-2 .9-2 2s.9 2 2 2 2-.9 2-2-.9-2-2-2z" />
  </svg>
)

export const IconSettings = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19.14 12.94c.04-.3.06-.61.06-.94 0-.32-.02-.64-.07-.94l2.03-1.58a.49.49 0 00.12-.61l-1.92-3.32a.488.488 0 00-.59-.22l-2.39.96c-.5-.38-1.03-.7-1.62-.94l-.36-2.54a.484.484 0 00-.48-.41h-3.84c-.24 0-.43.17-.47.41l-.36 2.54c-.59.24-1.13.57-1.62.94l-2.39-.96c-.22-.08-.47 0-.59.22L3.16 8.87c-.12.21-.08.47.12.61l2.03 1.58c-.05.3-.09.63-.09.94s.02.64.07.94l-2.03 1.58a.49.49 0 00-.12.61l1.92 3.32c.12.22.37.29.59.22l2.39-.96c.5.38 1.03.7 1.62.94l.36 2.54c.05.24.24.41.48.41h3.84c.24 0 .44-.17.47-.41l.36-2.54c.59-.24 1.13-.58 1.62-.94l2.39.96c.22.08.47 0 .59-.22l1.92-3.32c.12-.22.07-.47-.12-.61l-2.01-1.58zM12 15.6c-1.98 0-3.6-1.62-3.6-3.6s1.62-3.6 3.6-3.6 3.6 1.62 3.6 3.6-1.62 3.6-3.6 3.6z" />
  </svg>
)

// ─── FILES ───

export const IconFile = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6 2c-1.1 0-1.99.9-1.99 2L4 20c0 1.1.89 2 1.99 2H18c1.1 0 2-.9 2-2V8l-6-6H6zm7 7V3.5L18.5 9H13z" />
  </svg>
)

export const IconFileText = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M14 2H6c-1.1 0-1.99.9-1.99 2L4 20c0 1.1.89 2 1.99 2H18c1.1 0 2-.9 2-2V8l-6-6zm2 16H8v-2h8v2zm0-4H8v-2h8v2zm-3-5V3.5L18.5 9H13z" />
  </svg>
)

export const IconFolder = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M10 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V8c0-1.1-.9-2-2-2h-8l-2-2z" />
  </svg>
)

export const IconFolderOpen = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M20 6h-8l-2-2H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V8c0-1.1-.9-2-2-2zm0 12H4V8h16v10z" />
  </svg>
)

export const IconImage = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M21 19V5c0-1.1-.9-2-2-2H5c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2zM8.5 13.5l2.5 3.01L14.5 12l4.5 6H5l3.5-4.5z" />
  </svg>
)

export const IconCode = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M9.4 16.6L4.8 12l4.6-4.6L8 6l-6 6 6 6 1.4-1.4zm5.2 0l4.6-4.6-4.6-4.6L16 6l6 6-6 6-1.4-1.4z" />
  </svg>
)

export const IconTerminal = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M20 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 14H4V6h16v12zM7.5 13.5l-1 1 2 2 3-3-2-2-1 1 1 1-2 1zm8.5 1.5h-5v2h5v-2z" />
  </svg>
)

export const IconLayers = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M11.99 18.54l-7.37-5.73L3 14.07l9 7 9-7-1.63-1.27-7.38 5.74zM12 16l7.36-5.73L21 9l-9-7-9 7 1.63 1.27L12 16z" />
  </svg>
)

export const IconDatabase = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.48 2 2 3.79 2 6s4.48 4 10 4 10-1.79 10-4S17.52 2 12 2zm0 6c-4.41 0-8-1.79-8-4s3.59-4 8-4 8 1.79 8 4-3.59 4-8 4zm0 4c-4.41 0-8-1.79-8-4v2c0 2.21 3.59 4 8 4s8-1.79 8-4v-2c0 2.21-3.59 4-8 4zm0 4c-4.41 0-8-1.79-8-4v2c0 2.21 3.59 4 8 4s8-1.79 8-4v-2c0 2.21-3.59 4-8 4z" />
  </svg>
)

export const IconPaperclip = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M16.5 6v11.5c0 2.21-1.79 4-4 4s-4-1.79-4-4V5a2.5 2.5 0 015 0v10.5c0 .55-.45 1-1 1s-1-.45-1-1V6H10v9.5a2.5 2.5 0 005 0V5c0-2.21-1.79-4-4-4S7 2.79 7 5v12.5c0 3.04 2.46 5.5 5.5 5.5s5.5-2.46 5.5-5.5V6h-1.5z" />
  </svg>
)

// ─── COMMUNICATION ───

export const IconMessage = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M20 2H4c-1.1 0-1.99.9-1.99 2L2 22l4-4h14c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zm2 12H6l-2 2V4h16v10z" />
  </svg>
)

export const IconMail = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M20 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z" />
  </svg>
)

export const IconBell = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 22c1.1 0 2-.9 2-2h-4c0 1.1.89 2 2 2zm6-6v-5c0-3.07-1.64-5.64-4.5-6.32V4c0-.83-.67-1.5-1.5-1.5s-1.5.67-1.5 1.5v.68C7.63 5.36 6 7.92 6 11v5l-2 2v1h16v-1l-2-2z" />
  </svg>
)

export const IconUser = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 12c2.21 0 4-1.79 4-4s-1.79-4-4-4-4 1.79-4 4 1.79 4 4 4zm0 2c-2.67 0-8 1.34-8 4v2h16v-2c0-2.66-5.33-4-8-4z" />
  </svg>
)

export const IconUsers = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5c-1.66 0-3 1.34-3 3s1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5C6.34 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm8 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z" />
  </svg>
)

export const IconSend = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M2.01 21L23 12 2.01 3 2 10l15 2-15 2z" />
  </svg>
)

export const IconMic = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 14c1.66 0 3-1.34 3-3V5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3z" />
    <path d="M17 11c0 2.76-2.24 5-5 5s-5-2.24-5-5H5c0 3.53 2.61 6.43 6 6.92V21h2v-3.08c3.39-.49 6-3.39 6-6.92h-2z" />
  </svg>
)

// ─── STATUS ───

export const IconInfo = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm1 15h-2v-6h2v6zm0-8h-2V7h2v2z" />
  </svg>
)

export const IconAlert = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M1 21h22L12 2 1 21zm12-3h-2v-2h2v2zm0-4h-2v-4h2v4z" />
  </svg>
)

export const IconAlertCircle = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm1 15h-2v-2h2v2zm0-4h-2V7h2v6z" />
  </svg>
)

export const IconCheckCircle = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-2 15l-5-5 1.41-1.41L10 14.17l7.59-7.59L19 8l-9 9z" />
  </svg>
)

export const IconXCircle = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.47 2 2 6.47 2 12s4.47 10 10 10 10-4.47 10-10S17.53 2 12 2zm5 13.59L15.59 17 12 13.41 8.41 17 7 15.59 10.59 12 7 8.41 8.41 7 12 10.59 15.59 7 17 8.41 13.41 12 17 15.59z" />
  </svg>
)

export const IconClock = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z" />
  </svg>
)

export const IconLock = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M18 8h-1V6c0-2.76-2.24-5-5-5S7 3.24 7 6v2H6c-1.1 0-2 .9-2 2v10c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V10c0-1.1-.9-2-2-2zm-6 9c-1.1 0-2-.9-2-2s.9-2 2-2 2 .9 2 2-.9 2-2 2zm3.1-9H8.9V6c0-1.71 1.39-3.1 3.1-3.1 1.71 0 3.1 1.39 3.1 3.1v2z" />
  </svg>
)

export const IconUnlock = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 17c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zm6-9h-1V6c0-2.76-2.24-5-5-5S7 3.24 7 6h1.9c0-1.71 1.39-3.1 3.1-3.1 1.71 0 3.1 1.39 3.1 3.1v2H6c-1.1 0-2 .9-2 2v10c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V10c0-1.1-.9-2-2-2zm0 12H6V10h12v10z" />
  </svg>
)

export const IconEye = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 4.5C7 4.5 2.73 7.61 1 12c1.73 4.39 6 7.5 11 7.5s9.27-3.11 11-7.5c-1.73-4.39-6-7.5-11-7.5zM12 17c-2.76 0-5-2.24-5-5s2.24-5 5-5 5 2.24 5 5-2.24 5-5 5zm0-8c-1.66 0-3 1.34-3 3s1.34 3 3 3 3-1.34 3-3-1.34-3-3-3z" />
  </svg>
)

export const IconEyeOff = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 7c2.76 0 5 2.24 5 5 0 .65-.13 1.26-.36 1.83l2.92 2.92c1.51-1.26 2.7-2.89 3.43-4.75-1.73-4.39-6-7.5-11-7.5-1.4 0-2.74.25-3.98.7l2.16 2.16C10.74 7.13 11.35 7 12 7zM2 4.27l2.28 2.28.46.46A11.8 11.8 0 001 12c1.73 4.39 6 7.5 11 7.5 1.55 0 3.03-.3 4.38-.84l.42.42L19.73 22 21 20.73 3.27 3 2 4.27zM7.53 9.8l1.55 1.55c-.05.21-.08.43-.08.65 0 1.66 1.34 3 3 3 .22 0 .44-.03.65-.08l1.55 1.55c-.67.33-1.41.53-2.2.53-2.76 0-5-2.24-5-5 0-.79.2-1.53.53-2.2zm4.31-.78l3.15 3.15.02-.16c0-1.66-1.34-3-3-3l-.17.01z" />
  </svg>
)

export const IconZap = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M7 2v11h3v9l7-12h-4l4-8z" />
  </svg>
)

export const IconHeart = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54L12 21.35z" />
  </svg>
)

export const IconBookmark = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M17 3H7c-1.1 0-1.99.9-1.99 2L5 21l7-3 7 3V5c0-1.1-.9-2-2-2z" />
  </svg>
)

export const IconStar = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 17.27L18.18 21l-1.64-7.03L22 9.24l-7.19-.61L12 2 9.19 8.63 2 9.24l5.46 4.73L5.82 21z" />
  </svg>
)

export const IconFlag = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M14.4 6L14 4H5v17h2v-7h5.6l.4 2h7V6z" />
  </svg>
)

export const IconTag = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M21.41 11.58l-9-9C12.05 2.22 11.55 2 11 2H4c-1.1 0-2 .9-2 2v7c0 .55.22 1.05.59 1.42l9 9c.36.36.86.58 1.41.58.55 0 1.05-.22 1.41-.59l7-7c.37-.36.59-.86.59-1.41 0-.55-.23-1.06-.59-1.42zM5.5 7C4.67 7 4 6.33 4 5.5S4.67 4 5.5 4 7 4.67 7 5.5 6.33 7 5.5 7z" />
  </svg>
)

export const IconFilter = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 5H1v16c0 1.1.9 2 2 2h16v-2H3V5zm18-4H7c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2V3c0-1.1-.9-2-2-2zm0 16H7V3h14v14z" />
  </svg>
)

export const IconSort = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 18h6v-2H3v2zM3 6v2h18V6H3zm0 7h12v-2H3v2z" />
  </svg>
)

// ─── MEDIA ───

export const IconPlay = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M8 5v14l11-7z" />
  </svg>
)

export const IconPause = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z" />
  </svg>
)

export const IconStop = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6 6h12v12H6z" />
  </svg>
)

export const IconSkipBack = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6 6h2v12H6zm3.5 6l8.5 6V6z" />
  </svg>
)

export const IconSkipForward = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6 18l8.5-6L6 6v12zM16 6v12h2V6h-2z" />
  </svg>
)

export const IconVolume = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 9v6h4l5 5V4L7 9H3zm13.5 3c0-1.77-1.02-3.29-2.5-4.03v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z" />
  </svg>
)

export const IconVolumeOff = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M16.5 12c0-1.77-1.02-3.29-2.5-4.03v2.21l2.45 2.45c.03-.2.05-.41.05-.63zm2.5 0c0 .94-.2 1.82-.54 2.64l1.51 1.51C20.63 14.91 21 13.5 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06c1.38-.31 2.63-.95 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z" />
  </svg>
)

export const IconCamera = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <circle cx="12" cy="12" r="3.2" />
    <path d="M9 2L7.17 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2h-3.17L15 2H9zm3 16c-3.31 0-6-2.69-6-6s2.69-6 6-6 6 2.69 6 6-2.69 6-6 6z" />
  </svg>
)

export const IconMusic = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 3v10.55c-.59-.34-1.27-.55-2-.55-2.21 0-4 1.79-4 4s1.79 4 4 4 4-1.79 4-4V7h4V3h-6z" />
  </svg>
)

export const IconMapPin = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C8.13 2 5 5.13 5 9c0 5.25 7 13 7 13s7-7.75 7-13c0-3.87-3.13-7-7-7zm0 9.5c-1.38 0-2.5-1.12-2.5-2.5s1.12-2.5 2.5-2.5 2.5 1.12 2.5 2.5-1.12 2.5-2.5 2.5z" />
  </svg>
)

export const IconCalendar = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19 3h-1V1h-2v2H8V1H6v2H5c-1.11 0-1.99.9-1.99 2L3 19a2 2 0 002 2h14c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2zm0 16H5V8h14v11zM9 10H7v2h2v-2zm4 0h-2v2h2v-2zm4 0h-2v2h2v-2zm-8 4H7v2h2v-2zm4 0h-2v2h2v-2zm4 0h-2v2h2v-2z" />
  </svg>
)

export const IconGrid = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 3h8v8H3zm10 0h8v8h-8zM3 13h8v8H3zm10 0h8v8h-8z" />
  </svg>
)

export const IconList = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3 13h2v-2H3v2zm0 4h2v-2H3v2zm0-8h2V7H3v2zm4 4h14v-2H7v2zm0 4h14v-2H7v2zM7 7v2h14V7H7z" />
  </svg>
)

export const IconGlobe = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-1 17.93c-3.95-.49-7-3.85-7-7.93 0-.62.08-1.21.21-1.79L9 15v1c0 1.1.9 2 2 2v1.93zm6.9-2.54c-.26-.81-1-1.39-1.9-1.39h-1v-3c0-.55-.45-1-1-1H8v-2h2c.55 0 1-.45 1-1V7h2c1.1 0 2-.9 2-2v-.41c2.93 1.19 5 4.06 5 7.41 0 2.08-.8 3.97-2.1 5.39z" />
  </svg>
)

export const IconWifi = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M1 9l2 2c4.97-4.97 13.03-4.97 18 0l2-2C16.93 2.93 7.08 2.93 1 9zm8 8l3 3 3-3a4.237 4.237 0 00-6 0zm-4-4l2 2a7.074 7.074 0 0110 0l2-2C15.14 9.14 8.87 9.14 5 13z" />
  </svg>
)

export const IconBluetooth = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M17.71 7.71L12 2h-1v7.59L6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 11 14.41V22h1l5.71-5.71-4.3-4.29 4.3-4.29zM13 5.83l1.88 1.88L13 9.59V5.83zm1.88 10.46L13 18.17v-3.76l1.88 1.88z" />
  </svg>
)

export const IconSun = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M6.76 4.84l-1.8-1.79-1.41 1.41 1.79 1.79 1.42-1.41zM4 10.5H1v2h3v-2zm9-9.95h-2V3.5h2V.55zm7.45 3.91l-1.41-1.41-1.79 1.79 1.41 1.41 1.79-1.79zm-3.21 13.7l1.79 1.8 1.41-1.41-1.8-1.79-1.4 1.4zM20 10.5v2h3v-2h-3zm-8-5c-3.31 0-6 2.69-6 6s2.69 6 6 6 6-2.69 6-6-2.69-6-6-6zm-1 16.95h2V19.5h-2v2.95zm-7.45-3.91l1.41 1.41 1.79-1.8-1.41-1.41-1.79 1.8z" />
  </svg>
)

export const IconMoon = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M9 2c-1.05 0-2.05.16-3 .46 1.69 1.24 2.79 3.24 2.79 5.54 0 3.87-3.13 7-7 7-1.13 0-2.19-.27-3.13-.74C.48 16.89 4.43 20 9 20c5.52 0 10-4.48 10-10S14.52 2 9 2z" />
  </svg>
)

// ─── AGENT / AI ───

export const IconSparkles = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M7 2v11h3v9l7-12h-4l4-8z" />
  </svg>
)

export const IconBot = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M20 2H4c-1.1 0-2 .9-2 2v18l4-4h14c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zM8 11c-.55 0-1-.45-1-1s.45-1 1-1 1 .45 1 1-.45 1-1 1zm8 0c-.55 0-1-.45-1-1s.45-1 1-1 1 .45 1 1-.45 1-1 1zm-4 5c-2.76 0-5-2.01-5-4.5h10c0 2.49-2.24 4.5-5 4.5z" />
  </svg>
)

export const IconBrain = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C8 2 5 5 5 9c0 2 1 3.5 2.5 4.5V18c0 1.1.9 2 2 2h5c1.1 0 2-.9 2-2v-4.5C17 12.5 19 11 19 9c0-4-3-7-7-7zm-1 15h2v2h-2v-2zm-3-3h8v2H8v-2z" />
  </svg>
)

export const IconCpu = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M15 2h-2v3h2V2zm5 5h-3v2h3V7zm-10 0H7v2h3V7zM9 2H7v3h2V2zm9 15h-2v3h2v-3zm-5 0h-2v3h2v-3zm-5 0H6v3h2v-3zm13-5h-3v2h3v-2zm0 4h-3v2h3v-2zM6 11H3v2h3v-2zm13 0h-3v2h3v-2zM8 6h8v2H8V6zm0 10h8v2H8v-2zm0-8h8v6H8V8z" />
  </svg>
)

export const IconHash = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M20 10h-4V8.5c0-.83-.67-1.5-1.5-1.5h-3V4h-2v3H8.5c-.83 0-1.5.67-1.5 1.5v1.5H4v2h3v2.5c0 .83.67 1.5 1.5 1.5h3V20h2v-3h2.5c.83 0 1.5-.67 1.5-1.5V14h4v-2h-4V10zm-6 4h-4v-4h4v4z" />
  </svg>
)

export const IconLink = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M3.9 12c0-1.71 1.39-3.1 3.1-3.1h4V7H7c-2.76 0-5 2.24-5 5s2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1zM8 13h8v-2H8v2zm9-6h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1s-1.39 3.1-3.1 3.1h-4V17h4c2.76 0 5-2.24 5-5s-2.24-5-5-5z" />
  </svg>
)

export const IconLogOut = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M10.09 15.59L11.5 17l5-5-5-5-1.41 1.41L12.67 11H3v2h9.67l-2.58 2.59zM19 3H5a2 2 0 00-2 2v4h2V5h14v14H5v-4H3v4a2 2 0 002 2h14c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2z" />
  </svg>
)

export const IconLogIn = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M19 3h-14a2 2 0 00-2 2v4h2v-4h14v14h-14v-4h-2v4a2 2 0 002 2h14c1.1 0 2-.9 2-2v-14c0-1.1-.9-2-2-2zm-8 12.5l3.5-3.5-3.5-3.5v2.5h-9v2h9v2.5z" />
  </svg>
)

export const IconShield = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 1L3 5v6c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V5l-9-4z" />
  </svg>
)

export const IconActivity = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M13.5 5.5c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zM9.8 8.9L7 23h2.1l1.8-8 2.1 2v6h2v-7.5l-2.1-2 .6-3C14.8 12 16.8 13 19 13v-2c-1.9 0-3.5-1-4.3-2.4l-1-1.6c-.4-.6-1-1-1.7-1-.3 0-.5.1-.8.1L6 8.3V13h2V9.6l1.8-.7z" />
  </svg>
)

export const IconTrendingUp = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M16 6l2.29 2.29-4.88 4.88-4-4L2 16.59 3.41 18l6-6 4 4 6.3-6.29L22 12V6z" />
  </svg>
)

export const IconBarChart = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M5 9.2h3V19H5zM10.6 5h2.8v14h-2.8zm5.6 8H19v6h-2.8z" />
  </svg>
)

export const IconPieChart = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M11 2v20c-5.07-.5-9-4.79-9-10s3.93-9.5 9-10zm2.03 0v8.99H22c-.47-4.74-4.24-8.52-8.97-8.99zm0 11.01V22c4.74-.47 8.5-4.25 8.97-8.99h-8.97z" />
  </svg>
)

export const IconHelpCircle = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm1 17h-2v-2h2v2zm2.07-7.75l-.9.92C13.45 12.9 13 13.5 13 15h-2v-.5c0-1.1.45-2.1 1.17-2.83l1.24-1.26c.37-.36.59-.86.59-1.41 0-1.1-.9-2-2-2s-2 .9-2 2H8c0-2.21 1.79-4 4-4s4 1.79 4 4c0 .88-.36 1.68-.93 2.25z" />
  </svg>
)

export const IconMaximize = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M5 5h5v2H7v3H5V5zm9 0h5v5h-2V7h-3V5zm3 9h2v5h-5v-2h3v-3zm-7 3v2H5v-5h2v3h3z" />
  </svg>
)

export const IconMinimize = (props: React.SVGProps<SVGSVGElement>) => (
  <svg {...p} {...props}>
    <path d="M5 11h5v5H8v-3H5v-2zm9-5v2h-3V5h5v5h-2V6zm-3 7h3v5h-5v-2h3v-3H8v-2h3zm7-2h-2v3h-3v2h5v-5z" />
  </svg>
)
