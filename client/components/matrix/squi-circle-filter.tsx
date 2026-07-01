/**
 * SquiCircleFilter — a reusable "gooey" SVG filter definition.
 *
 * It renders no visible pixels of its own; it only registers an SVG filter
 * (`#SquiCircleFilter`) in the DOM. Apply it to any element with
 * `style={{ filter: 'url(#SquiCircleFilter)' }}` (or the Tailwind
 * `[filter:url(#SquiCircleFilter)]` arbitrary property) to merge overlapping
 * shapes into soft, rounded blobs while keeping the source graphic crisp on top.
 *
 * Mount once near the root (see app/layout.tsx) so every consumer can reference it.
 */
export const SquiCircleFilter = ({
  blurValue = 10,
  colorMatrixValue = 20,
  alphaValue = -7,
}: {
  blurValue?: number
  colorMatrixValue?: number
  alphaValue?: number
}) => {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      className="pointer-events-none absolute bottom-0 left-0 h-0 w-0"
      aria-hidden="true"
      focusable="false"
      version="1.1"
    >
      <defs>
        <filter id="SquiCircleFilter">
          <feGaussianBlur in="SourceGraphic" stdDeviation={blurValue} result="blur" />
          <feColorMatrix
            in="blur"
            mode="matrix"
            values={`1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 ${colorMatrixValue} ${alphaValue}`}
            result="goo"
          />
          <feBlend in="SourceGraphic" in2="goo" />
        </filter>
      </defs>
    </svg>
  )
}
