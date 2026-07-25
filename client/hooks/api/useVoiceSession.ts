'use client'

import { useCallback, useEffect, useRef, useState } from 'react'
import { Room, RoomEvent, Track, type Participant } from 'livekit-client'
import { useAudioDevices } from '@/components/ai-elements/mic-selector'
import { getVoiceToken, stopVoiceSession } from '@/lib/api/voice'
import type { VoicePrefs } from '@/lib/prefs'

export type VoiceAgentState = 'connecting' | 'listening' | 'thinking' | 'speaking'

function stateFor(participant?: Participant): VoiceAgentState {
  const state = participant?.attributes['lk.agent.state']
  return state === 'listening' || state === 'thinking' || state === 'speaking'
    ? state
    : 'connecting'
}

export function useVoiceSession({
  conversationId,
  settings,
  onIntent,
  unavailableNotice,
  firstTurnNotice,
}: {
  conversationId?: string | null
  settings: VoicePrefs
  onIntent?: (intentId: string) => void
  unavailableNotice: string
  firstTurnNotice: string
}) {
  const { devices, loadDevices } = useAudioDevices()
  const [active, setActive] = useState(false)
  const [state, setState] = useState<VoiceAgentState>('connecting')
  const [notice, setNotice] = useState('')
  const [deviceId, setDeviceId] = useState('')
  const roomRef = useRef<Room | null>(null)
  const intentionalRef = useRef(false)
  const onIntentRef = useRef(onIntent)

  useEffect(() => {
    onIntentRef.current = onIntent
  }, [onIntent])

  useEffect(() => {
    if (!deviceId && devices[0]?.deviceId) setDeviceId(devices[0].deviceId)
  }, [deviceId, devices])

  const stop = useCallback(
    async (notifyServer = true) => {
      intentionalRef.current = true
      const room = roomRef.current
      roomRef.current = null
      if (room) await room.disconnect().catch(() => {})
      if (notifyServer && room && conversationId) {
        await stopVoiceSession(conversationId).catch(() => {})
      }
      setActive(false)
      setState('connecting')
    },
    [conversationId],
  )

  const start = useCallback(async () => {
    if (!conversationId) {
      setNotice(firstTurnNotice)
      return
    }
    setNotice('')
    setState('connecting')
    intentionalRef.current = false
    const room = new Room({ adaptiveStream: true, dynacast: true })
    roomRef.current = room
    const attached = new Set<HTMLMediaElement>()
    const updateAgent = (participant?: Participant) => {
      if (participant?.identity === 'neo-voice') setState(stateFor(participant))
    }

    room.on(RoomEvent.ParticipantConnected, updateAgent)
    room.on(RoomEvent.ParticipantAttributesChanged, (_changed, participant) =>
      updateAgent(participant),
    )
    room.on(RoomEvent.ParticipantDisconnected, (participant) => {
      if (participant.identity === 'neo-voice' && !intentionalRef.current) {
        setNotice(unavailableNotice)
        setActive(false)
      }
    })
    room.on(RoomEvent.TrackSubscribed, (track) => {
      if (track.kind !== Track.Kind.Audio) return
      const element = track.attach()
      element.autoplay = true
      element.style.display = 'none'
      attached.add(element)
      document.body.appendChild(element)
    })
    room.on(RoomEvent.TrackUnsubscribed, (track) => {
      for (const element of track.detach()) {
        attached.delete(element)
        element.remove()
      }
    })
    room.on(RoomEvent.DataReceived, (payload, _participant, _kind, topic) => {
      if (topic !== 'neo.voice.intent') return
      try {
        const parsed = JSON.parse(new TextDecoder().decode(payload)) as { intent_id?: unknown }
        if (typeof parsed.intent_id === 'string' && parsed.intent_id) {
          onIntentRef.current?.(parsed.intent_id)
        }
      } catch {
        return
      }
    })
    room.on(RoomEvent.MediaDevicesError, () => setNotice(unavailableNotice))
    room.on(RoomEvent.Disconnected, () => {
      for (const element of attached) element.remove()
      attached.clear()
      if (!intentionalRef.current) setNotice(unavailableNotice)
      setActive(false)
    })

    const autoplay = room.startAudio().catch(() => {})
    try {
      await loadDevices()
      const token = await getVoiceToken(conversationId, settings)
      await room.connect(token.server_url, token.token)
      await autoplay
      await room.localParticipant.setMicrophoneEnabled(true, deviceId ? { deviceId } : undefined)
      const agent = room.getParticipantByIdentity('neo-voice')
      setState(agent ? stateFor(agent) : 'connecting')
      setActive(true)
    } catch {
      intentionalRef.current = true
      roomRef.current = null
      await room.disconnect().catch(() => {})
      await stopVoiceSession(conversationId).catch(() => {})
      setActive(false)
      setNotice(unavailableNotice)
    }
  }, [conversationId, deviceId, firstTurnNotice, loadDevices, settings, unavailableNotice])

  const toggle = useCallback(() => {
    if (active || roomRef.current) void stop()
    else void start()
  }, [active, start, stop])

  const selectDevice = useCallback((next: string) => {
    setDeviceId(next)
    const room = roomRef.current
    if (room) void room.switchActiveDevice('audioinput', next).catch(() => {})
  }, [])

  useEffect(
    () => () => {
      void stop()
    },
    [stop],
  )

  return { active, state, notice, devices, deviceId, selectDevice, toggle }
}
