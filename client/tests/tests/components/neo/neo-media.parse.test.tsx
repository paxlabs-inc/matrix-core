import { describe, expect, it } from 'vitest'
import { parseAttachments } from '@/components/matrix/neo/neo-media'

describe('parseAttachments', () => {
  it('parses media markers without a name', () => {
    const { clean, items } = parseAttachments(
      'look at this\n\n[attached image: /media/abc.png]\n[attached audio: /media/x.mp3]',
    )
    expect(items).toEqual([
      { url: '/media/abc.png', kind: 'image', name: undefined },
      { url: '/media/x.mp3', kind: 'audio', name: undefined },
    ])
    expect(clean).toBe('look at this')
  })

  it('parses document markers carrying the original filename', () => {
    const { clean, items } = parseAttachments(
      'summarize this\n\n[attached file: /media/20260711aa.pdf (Q2 report.pdf)]',
    )
    expect(items).toEqual([{ url: '/media/20260711aa.pdf', kind: 'file', name: 'Q2 report.pdf' }])
    expect(clean).toBe('summarize this')
  })

  it('parses a document marker without a filename suffix', () => {
    const { items } = parseAttachments('[attached file: /media/z.bin]')
    expect(items).toEqual([{ url: '/media/z.bin', kind: 'file', name: undefined }])
  })
})
