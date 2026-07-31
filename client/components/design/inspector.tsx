'use client'

/**
 * Design Mode — inspector panel.
 *
 * Edits the selected element's style at the ACTIVE breakpoint. Inputs show the
 * value set directly on that breakpoint (empty = inherited); placeholders show
 * the live computed value so you always see the real current render.
 */
import { useState } from 'react'
import { ColorField, LenInput, Row, Seg, Section, SidesField } from './controls'
import { useSelectedElement } from './hooks'
import { bpPrefix } from '@/lib/design/breakpoints'
import { useDesignStore } from '@/lib/design/store'
import { ownValue } from '@/lib/design/values'

type Tab = 'layout' | 'size' | 'space' | 'position' | 'text' | 'surface'
const TABS: { key: Tab; label: string }[] = [
  { key: 'layout', label: 'Layout' },
  { key: 'size', label: 'Size' },
  { key: 'space', label: 'Space' },
  { key: 'position', label: 'Pos' },
  { key: 'text', label: 'Text' },
  { key: 'surface', label: 'Fill' },
]

export function Inspector() {
  const enabled = useDesignStore((s) => s.enabled)
  const selected = useDesignStore((s) => s.selected)
  const bp = useDesignStore((s) => s.activeBreakpoint)
  const overrides = useDesignStore((s) => s.overrides)
  const setProp = useDesignStore((s) => s.setProp)
  const clearSelector = useDesignStore((s) => s.clearSelector)
  const clearBreakpoint = useDesignStore((s) => s.clearBreakpoint)
  const { el, computed } = useSelectedElement()
  const [tab, setTab] = useState<Tab>('layout')

  if (!enabled) return null

  const own = (prop: string): string | undefined => ownValue(overrides, selected, bp, prop)
  const set = (prop: string, v: string | null) => {
    if (selected) setProp(selected, bp, prop, v)
  }
  // Seg controls: reflect direct override, else the live computed value.
  const cur = (prop: string): string | undefined =>
    own(prop) ?? (selected ? computed(prop) : undefined)

  const prefix = bpPrefix(bp)
  const overriddenHere = selected ? !!overrides[selected]?.[bp] : false

  return (
    <aside
      data-mx-ui="true"
      className="fixed top-16 right-3 bottom-3 z-[2147483601] flex w-[300px] flex-col overflow-hidden rounded-lg bg-[#0a0a0b] text-white shadow-2xl"
    >
      {!selected || !el ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 px-6 text-center">
          <div className="text-[13px] font-medium text-[#E4E4E7]">Nothing selected</div>
          <div className="text-[11px] leading-relaxed text-[#71717A]">
            Click any component on the page to select it, then style, move, and resize it here.
          </div>
        </div>
      ) : (
        <>
          {/* header */}
          <div className="flex items-center justify-between gap-2 bg-[#111113] px-3 py-2">
            <div className="min-w-0">
              <div className="truncate font-mono text-[11px] text-[#E4E4E7]" title={selected ?? ''}>
                {el.tagName.toLowerCase()}
              </div>
              <div className="truncate text-[10px] text-[#52525B]" title={selected ?? ''}>
                {selected}
              </div>
            </div>
            <button
              type="button"
              onClick={() => clearSelector(selected)}
              className="shrink-0 rounded bg-[#1c1c1f] px-2 py-1 text-[10px] font-medium text-[#A1A1AA] hover:text-white"
              title="Remove all overrides on this element"
            >
              Clear
            </button>
          </div>

          {/* editing-at banner */}
          <div className="flex items-center justify-between bg-[#0d0d0f] px-3 py-1.5 text-[10px]">
            <span className="text-[#71717A]">
              Editing <span className="font-mono text-[#004CED]">{prefix || 'base'}</span>
            </span>
            {overriddenHere ? (
              <button
                type="button"
                onClick={() => clearBreakpoint(selected, bp)}
                className="text-[#71717A] hover:text-white"
              >
                Reset {bp}
              </button>
            ) : null}
          </div>

          {/* tabs */}
          <div className="flex items-center gap-0.5 px-2 pt-2">
            {TABS.map((t) => (
              <button
                key={t.key}
                type="button"
                onClick={() => setTab(t.key)}
                className="flex-1 rounded-[4px] px-1 py-1 text-[10px] font-medium transition-colors"
                style={
                  tab === t.key
                    ? { backgroundColor: '#004CED', color: '#fff' }
                    : { color: '#A1A1AA' }
                }
              >
                {t.label}
              </button>
            ))}
          </div>

          <div className="flex-1 space-y-2 overflow-y-auto p-2">
            {tab === 'layout' && <LayoutTab cur={cur} own={own} set={set} />}
            {tab === 'size' && <SizeTab own={own} computed={computed} set={set} />}
            {tab === 'space' && <SpaceTab own={own} computed={computed} set={set} />}
            {tab === 'position' && (
              <PositionTab cur={cur} own={own} computed={computed} set={set} />
            )}
            {tab === 'text' && <TextTab cur={cur} own={own} computed={computed} set={set} />}
            {tab === 'surface' && <SurfaceTab own={own} computed={computed} set={set} />}
          </div>
        </>
      )}
    </aside>
  )
}

interface FieldApi {
  own: (p: string) => string | undefined
  set: (p: string, v: string | null) => void
  computed: (p: string) => string
}
interface SegApi {
  cur: (p: string) => string | undefined
  own: (p: string) => string | undefined
  set: (p: string, v: string | null) => void
}

function LayoutTab({ cur, own, set }: SegApi) {
  const display = cur('display')
  const isFlex = display === 'flex' || display === 'inline-flex'
  return (
    <>
      <Section title="Display">
        <Seg
          full
          value={display as string | undefined}
          onChange={(v) => set('display', v)}
          options={[
            { value: 'block', label: 'Block' },
            { value: 'flex', label: 'Flex' },
            { value: 'grid', label: 'Grid' },
            { value: 'inline-flex', label: 'Inline' },
            { value: 'none', label: 'None' },
          ]}
        />
      </Section>
      {isFlex && (
        <Section title="Flex">
          <Row label="Direction">
            <Seg
              full
              value={cur('flex-direction') as string | undefined}
              onChange={(v) => set('flex-direction', v)}
              options={[
                { value: 'row', label: 'Row' },
                { value: 'column', label: 'Col' },
              ]}
            />
          </Row>
          <Row label="Justify">
            <Seg
              full
              value={cur('justify-content') as string | undefined}
              onChange={(v) => set('justify-content', v)}
              options={[
                { value: 'flex-start', label: 'S' },
                { value: 'center', label: 'C' },
                { value: 'flex-end', label: 'E' },
                { value: 'space-between', label: '⇿' },
              ]}
            />
          </Row>
          <Row label="Align">
            <Seg
              full
              value={cur('align-items') as string | undefined}
              onChange={(v) => set('align-items', v)}
              options={[
                { value: 'flex-start', label: 'S' },
                { value: 'center', label: 'C' },
                { value: 'flex-end', label: 'E' },
                { value: 'stretch', label: 'St' },
              ]}
            />
          </Row>
          <Row label="Wrap">
            <Seg
              full
              value={cur('flex-wrap') as string | undefined}
              onChange={(v) => set('flex-wrap', v)}
              options={[
                { value: 'nowrap', label: 'No' },
                { value: 'wrap', label: 'Wrap' },
              ]}
            />
          </Row>
          <Row label="Gap">
            <LenInput value={own('gap')} placeholder="0px" onChange={(v) => set('gap', v)} />
          </Row>
        </Section>
      )}
      <Section title="Order">
        <Row label="Order">
          <LenInput value={own('order')} placeholder="0" onChange={(v) => set('order', v)} />
        </Row>
      </Section>
    </>
  )
}

function SizeTab({ own, computed, set }: FieldApi) {
  return (
    <Section title="Dimensions">
      <Row label="Width">
        <LenInput
          value={own('width')}
          placeholder={computed('width')}
          onChange={(v) => set('width', v)}
        />
      </Row>
      <Row label="Height">
        <LenInput
          value={own('height')}
          placeholder={computed('height')}
          onChange={(v) => set('height', v)}
        />
      </Row>
      <Row label="Min W">
        <LenInput value={own('min-width')} placeholder="0" onChange={(v) => set('min-width', v)} />
      </Row>
      <Row label="Max W">
        <LenInput
          value={own('max-width')}
          placeholder="none"
          onChange={(v) => set('max-width', v)}
        />
      </Row>
      <Row label="Min H">
        <LenInput
          value={own('min-height')}
          placeholder="0"
          onChange={(v) => set('min-height', v)}
        />
      </Row>
      <Row label="Max H">
        <LenInput
          value={own('max-height')}
          placeholder="none"
          onChange={(v) => set('max-height', v)}
        />
      </Row>
    </Section>
  )
}

function SpaceTab({ own, computed, set }: FieldApi) {
  return (
    <>
      <Section title="Padding">
        <SidesField
          values={{
            top: own('padding-top'),
            right: own('padding-right'),
            bottom: own('padding-bottom'),
            left: own('padding-left'),
          }}
          placeholders={{
            top: computed('padding-top'),
            right: computed('padding-right'),
            bottom: computed('padding-bottom'),
            left: computed('padding-left'),
          }}
          onChange={(side, v) => set(`padding-${side}`, v)}
        />
      </Section>
      <Section title="Margin">
        <SidesField
          values={{
            top: own('margin-top'),
            right: own('margin-right'),
            bottom: own('margin-bottom'),
            left: own('margin-left'),
          }}
          placeholders={{
            top: computed('margin-top'),
            right: computed('margin-right'),
            bottom: computed('margin-bottom'),
            left: computed('margin-left'),
          }}
          onChange={(side, v) => set(`margin-${side}`, v)}
        />
      </Section>
    </>
  )
}

function PositionTab({ cur, own, computed, set }: SegApi & { computed: (p: string) => string }) {
  return (
    <>
      <Section title="Position">
        <Seg
          full
          value={cur('position') as string | undefined}
          onChange={(v) => set('position', v)}
          options={[
            { value: 'static', label: 'Static' },
            { value: 'relative', label: 'Rel' },
            { value: 'absolute', label: 'Abs' },
            { value: 'fixed', label: 'Fix' },
            { value: 'sticky', label: 'Stick' },
          ]}
        />
        <div className="mt-1.5 grid grid-cols-2 gap-1.5">
          <Row label="Top">
            <LenInput value={own('top')} placeholder="auto" onChange={(v) => set('top', v)} />
          </Row>
          <Row label="Right">
            <LenInput value={own('right')} placeholder="auto" onChange={(v) => set('right', v)} />
          </Row>
          <Row label="Bottom">
            <LenInput value={own('bottom')} placeholder="auto" onChange={(v) => set('bottom', v)} />
          </Row>
          <Row label="Left">
            <LenInput value={own('left')} placeholder="auto" onChange={(v) => set('left', v)} />
          </Row>
        </div>
        <Row label="Z-index">
          <LenInput
            value={own('z-index')}
            placeholder={computed('z-index')}
            onChange={(v) => set('z-index', v)}
          />
        </Row>
      </Section>
      <Section title="Transform">
        <Row label="Transform" hint="e.g. translate(10px, 20px) rotate(4deg)">
          <LenInput
            value={own('transform')}
            placeholder="none"
            onChange={(v) => set('transform', v)}
          />
        </Row>
        <div className="px-0.5 pt-1 text-[10px] leading-relaxed text-[#52525B]">
          Tip: drag the element in Move mode to set translate visually.
        </div>
      </Section>
    </>
  )
}

function TextTab({ cur, own, computed, set }: SegApi & { computed: (p: string) => string }) {
  return (
    <>
      <Section title="Typography">
        <Row label="Size">
          <LenInput
            value={own('font-size')}
            placeholder={computed('font-size')}
            onChange={(v) => set('font-size', v)}
          />
        </Row>
        <Row label="Weight">
          <LenInput
            value={own('font-weight')}
            placeholder={computed('font-weight')}
            onChange={(v) => set('font-weight', v)}
          />
        </Row>
        <Row label="Line H">
          <LenInput
            value={own('line-height')}
            placeholder={computed('line-height')}
            onChange={(v) => set('line-height', v)}
          />
        </Row>
        <Row label="Spacing">
          <LenInput
            value={own('letter-spacing')}
            placeholder="normal"
            onChange={(v) => set('letter-spacing', v)}
          />
        </Row>
        <Row label="Align">
          <Seg
            full
            value={cur('text-align') as string | undefined}
            onChange={(v) => set('text-align', v)}
            options={[
              { value: 'left', label: 'L' },
              { value: 'center', label: 'C' },
              { value: 'right', label: 'R' },
              { value: 'justify', label: 'J' },
            ]}
          />
        </Row>
      </Section>
      <Section title="Color">
        <ColorField
          value={own('color')}
          placeholder={computed('color')}
          onChange={(v) => set('color', v)}
        />
      </Section>
    </>
  )
}

function SurfaceTab({ own, computed, set }: FieldApi) {
  return (
    <>
      <Section title="Background">
        <ColorField
          value={own('background-color')}
          placeholder={computed('background-color')}
          onChange={(v) => set('background-color', v)}
        />
      </Section>
      <Section title="Shape">
        <Row label="Radius">
          <LenInput
            value={own('border-radius')}
            placeholder={computed('border-radius')}
            onChange={(v) => set('border-radius', v)}
          />
        </Row>
        <Row label="Opacity">
          <LenInput
            value={own('opacity')}
            placeholder={computed('opacity')}
            onChange={(v) => set('opacity', v)}
          />
        </Row>
        <Row label="Overflow">
          <Seg
            full
            value={own('overflow')}
            onChange={(v) => set('overflow', v)}
            options={[
              { value: 'visible', label: 'Vis' },
              { value: 'hidden', label: 'Hide' },
              { value: 'auto', label: 'Auto' },
              { value: 'scroll', label: 'Scrl' },
            ]}
          />
        </Row>
        <Row label="Shadow" hint="full CSS box-shadow value">
          <LenInput
            value={own('box-shadow')}
            placeholder="none"
            onChange={(v) => set('box-shadow', v)}
          />
        </Row>
      </Section>
    </>
  )
}
