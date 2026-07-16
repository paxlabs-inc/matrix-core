'use client'

/**
 * NeoBrain3D — WebGL neural-network rendering of the codegraph self-model.
 *
 * Loads the GLB neural network model (artificial_neural_network_ann.glb) as
 * the visual backbone — its sweep mesh provides the wiring, and 54 neuron
 * cube meshes from the Cloner groups represent codegraph packages. Each neuron
 * is colored by module and clickable. UnrealBloom gives synaptic glow;
 * OrbitControls give free orbit/zoom; a slow auto-rotation keeps the network
 * alive until the user grabs it.
 *
 * All coordinates are in GLB native space (±1000 XY, 0 to -6000 Z).
 */
import { useEffect, useRef } from 'react'
import * as THREE from 'three'
import { OrbitControls } from 'three/addons/controls/OrbitControls.js'
import { EffectComposer } from 'three/addons/postprocessing/EffectComposer.js'
import { RenderPass } from 'three/addons/postprocessing/RenderPass.js'
import { UnrealBloomPass } from 'three/addons/postprocessing/UnrealBloomPass.js'
import { OutputPass } from 'three/addons/postprocessing/OutputPass.js'
import { GLTFLoader } from 'three/addons/loaders/GLTFLoader.js'
import {
  mapPackagesToNeurons,
  findNeuronForNode,
  selectionEdges,
  type NeuronLayout,
  type NeuronMapping,
  type SelfModelGraph,
} from './self-model-graph'

export interface NeoBrain3DProps {
  graph: SelfModelGraph
  /** currently selected node index, -1 for none */
  selected: number
  onSelect: (idx: number) => void
  /** module index to spotlight, -1 for all */
  moduleFilter: number
  reducedMotion: boolean
}

/* ------------------------------- constants -------------------------- */

const GLB_URL = '/artificial_neural_network_ann.glb'

const MODULE_COLORS: Record<number, THREE.Color> = {
  0: new THREE.Color('#2fe6b0'), // cody
  1: new THREE.Color('#c07bff'), // cortex
  2: new THREE.Color('#ffb84d'), // executor
  3: new THREE.Color('#5b8cff'), // neo
}

/* ------------------------------- helpers ---------------------------- */

/** Synapse particles that travel between neurons across layers. */
interface SynapseField {
  points: THREE.Points
  update: (dt: number) => void
}

function buildSynapses(layout: NeuronLayout, count = 300): SynapseField {
  const edges: [number, number][] = []
  const layerStarts = [0, 25, 34, 50]
  for (let layer = 0; layer < 3; layer++) {
    const fromStart = layerStarts[layer]
    const fromEnd = layerStarts[layer + 1]
    const toStart = layerStarts[layer + 1]
    const toEnd = layerStarts[layer + 2] ?? layout.count
    for (let a = fromStart; a < fromEnd; a++) {
      for (let b = toStart; b < toEnd; b++) {
        edges.push([a, b])
      }
    }
  }

  const rand = () => Math.random()
  const pos = new Float32Array(count * 3)
  const from = new Int32Array(count)
  const to = new Int32Array(count)
  const t = new Float32Array(count)
  const speed = new Float32Array(count)

  const respawn = (i: number) => {
    const pair = edges[(rand() * edges.length) | 0] ?? [0, 0]
    from[i] = pair[0]
    to[i] = pair[1]
    t[i] = 0
    speed[i] = 0.25 + rand() * 0.6
  }
  for (let i = 0; i < count; i++) {
    respawn(i)
    t[i] = rand()
  }

  const geo = new THREE.BufferGeometry()
  geo.setAttribute('position', new THREE.BufferAttribute(pos, 3))
  const mat = new THREE.PointsMaterial({
    color: new THREE.Color('#dbe6ff'),
    size: 0.15,
    transparent: true,
    opacity: 0.85,
    blending: THREE.AdditiveBlending,
    depthWrite: false,
    sizeAttenuation: true,
    toneMapped: false,
  })
  const points = new THREE.Points(geo, mat)
  points.raycast = () => {}

  const p = layout.positions
  const update = (dt: number) => {
    for (let i = 0; i < count; i++) {
      t[i] += dt * speed[i]
      if (t[i] >= 1) respawn(i)
      const a = from[i] * 3
      const b = to[i] * 3
      const k = t[i]
      const lift = Math.sin(k * Math.PI) * 0.15
      pos[i * 3] = p[a] + (p[b] - p[a]) * k
      pos[i * 3 + 1] = p[a + 1] + (p[b + 1] - p[a + 1]) * k + lift
      pos[i * 3 + 2] = p[a + 2] + (p[b + 2] - p[a + 2]) * k
    }
    geo.attributes.position.needsUpdate = true
  }
  return { points, update }
}

/** Ambient dust for depth perception. */
function buildDust(count = 500): THREE.Points {
  const pos = new Float32Array(count * 3)
  for (let i = 0; i < count; i++) {
    const r = 15 + Math.random() * 30
    const theta = Math.random() * Math.PI * 2
    const phi = Math.acos(2 * Math.random() - 1)
    pos[i * 3] = r * Math.sin(phi) * Math.cos(theta)
    pos[i * 3 + 1] = r * Math.cos(phi)
    pos[i * 3 + 2] = r * Math.sin(phi) * Math.sin(theta) - 30
  }
  const geo = new THREE.BufferGeometry()
  geo.setAttribute('position', new THREE.BufferAttribute(pos, 3))
  const mat = new THREE.PointsMaterial({
    color: new THREE.Color('#44506a'),
    size: 0.06,
    transparent: true,
    opacity: 0.4,
    depthWrite: false,
    sizeAttenuation: true,
  })
  const points = new THREE.Points(geo, mat)
  points.raycast = () => {}
  return points
}

/* ------------------------------ component --------------------------- */

export default function NeoBrain3D({
  graph,
  selected,
  onSelect,
  moduleFilter,
  reducedMotion,
}: NeoBrain3DProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const onSelectRef = useRef(onSelect)
  onSelectRef.current = onSelect

  // Scene handles exposed to the prop-sync effects below.
  const sceneRef = useRef<{
    neuronMeshes: THREE.Mesh[]
    glbScene: THREE.Group | null
    layout: NeuronLayout
    mappings: NeuronMapping[]
    edgeGroup: THREE.Group
    controls: OrbitControls
    focus: (neuronIdx: number) => void
    setDimming: (moduleFilter: number, selectedNeuron: number) => void
  } | null>(null)

  /* -------- scene lifecycle (mount once per graph) -------- */
  useEffect(() => {
    const host = hostRef.current
    if (!host) return

    const layout = mapPackagesToNeurons(graph)

    const scene = new THREE.Scene()
    scene.fog = new THREE.FogExp2(new THREE.Color('#05070d').getHex(), 0.02)

    // Camera positioned to see the full GLB model (world space: ±10 XY, Z -60..0).
    const camera = new THREE.PerspectiveCamera(45, 1, 0.1, 200)
    camera.position.set(0, 5, 20)

    const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: false })
    renderer.setClearColor(new THREE.Color('#05070d'))
    renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2))
    host.appendChild(renderer.domElement)
    renderer.domElement.style.display = 'block'
    renderer.domElement.style.touchAction = 'none'

    // --- load GLB model (native scale, no shrinking) ---
    const loader = new GLTFLoader()
    let glbScene: THREE.Group | null = null
    const neuronMeshes: THREE.Mesh[] = []

    loader.load(
      GLB_URL,
      (gltf) => {
        glbScene = gltf.scene
        // NO scale — work in GLB native coordinate space.

        const clonerNames = new Set(['Cloner', 'Cloner_1', 'Cloner_2', 'Cloner_3'])

        glbScene.traverse((obj) => {
          if (!(obj instanceof THREE.Mesh)) return

          // Identify neuron cubes by walking up to a Cloner parent.
          let parent: THREE.Object3D | null = obj.parent
          let layerIdx = -1
          while (parent) {
            if (clonerNames.has(parent.name)) {
              layerIdx =
                parent.name === 'Cloner'
                  ? 0
                  : parent.name === 'Cloner_1'
                    ? 1
                    : parent.name === 'Cloner_2'
                      ? 2
                      : 3
              break
            }
            parent = parent.parent
          }

          if (layerIdx >= 0) {
            // Neuron cube — color by module.
            const layerModule = [0, 2, 3, 1]
            const modIdx = layerModule[layerIdx]
            const color = MODULE_COLORS[modIdx] ?? new THREE.Color('#9aa4b2')
            obj.material = new THREE.MeshStandardMaterial({
              color: color,
              emissive: color,
              emissiveIntensity: 0.7,
              metalness: 0.2,
              roughness: 0.5,
              toneMapped: false,
            })
            obj.userData.neuronIdx = neuronMeshes.length
            neuronMeshes.push(obj)
          }
          // Sweep wiring and everything else — leave untouched.
        })

        scene.add(glbScene)
      },
      undefined,
      () => {},
    )

    // --- ambient ---
    const dust = buildDust()
    scene.add(dust)
    const synapses = buildSynapses(layout)
    scene.add(synapses.points)
    const edgeGroup = new THREE.Group()
    scene.add(edgeGroup)

    // --- controls ---
    const controls = new OrbitControls(camera, renderer.domElement)
    controls.enableDamping = true
    controls.dampingFactor = 0.08
    controls.minDistance = 2
    controls.maxDistance = 80
    controls.autoRotate = !reducedMotion
    controls.autoRotateSpeed = 0.45
    controls.target.set(0, 0, -30)
    const stopAutoRotate = () => {
      controls.autoRotate = false
    }
    controls.addEventListener('start', stopAutoRotate)

    // --- post-processing ---
    const composer = new EffectComposer(renderer)
    composer.addPass(new RenderPass(scene, camera))
    const bloom = new UnrealBloomPass(new THREE.Vector2(1, 1), 0.8, 0.4, 0.15)
    composer.addPass(bloom)
    composer.addPass(new OutputPass())

    // --- sizing ---
    const resize = () => {
      const w = host.clientWidth || 1
      const h = host.clientHeight || 1
      camera.aspect = w / h
      camera.updateProjectionMatrix()
      renderer.setSize(w, h)
      composer.setSize(w, h)
      bloom.setSize(w, h)
    }
    resize()
    const ro = new ResizeObserver(resize)
    ro.observe(host)

    // --- picking ---
    const raycaster = new THREE.Raycaster()
    const pointer = new THREE.Vector2()
    let downAt: [number, number] | null = null
    const onPointerDown = (e: PointerEvent) => {
      downAt = [e.clientX, e.clientY]
    }
    const onPointerUp = (e: PointerEvent) => {
      if (!downAt) return
      const dx = e.clientX - downAt[0]
      const dy = e.clientY - downAt[1]
      downAt = null
      if (dx * dx + dy * dy > 25) return
      const rect = renderer.domElement.getBoundingClientRect()
      pointer.x = ((e.clientX - rect.left) / rect.width) * 2 - 1
      pointer.y = -((e.clientY - rect.top) / rect.height) * 2 + 1
      raycaster.setFromCamera(pointer, camera)
      raycaster.params.Points = { threshold: 0 }
      const hits = raycaster.intersectObjects(neuronMeshes, false)
      if (hits.length > 0) {
        const neuronIdx = hits[0].object.userData.neuronIdx as number
        const pkgs = layout.mappings[neuronIdx]?.packageIndices
        onSelectRef.current(pkgs?.[0] ?? -1)
      } else {
        onSelectRef.current(-1)
      }
    }
    renderer.domElement.addEventListener('pointerdown', onPointerDown)
    renderer.domElement.addEventListener('pointerup', onPointerUp)

    // --- focus fly-to ---
    const focusTarget = new THREE.Vector3()
    let focusing = false
    const focus = (neuronIdx: number) => {
      if (neuronIdx < 0 || neuronIdx >= neuronMeshes.length) return
      const mesh = neuronMeshes[neuronIdx]
      const worldPos = new THREE.Vector3()
      mesh.getWorldPosition(worldPos)
      focusTarget.copy(worldPos)
      focusing = true
      controls.autoRotate = false
    }

    // --- dim / highlight recolor ---
    const setDimming = (modFilter: number, selectedNode: number) => {
      const selNeuron =
        selectedNode >= 0 ? findNeuronForNode(selectedNode, graph, layout.mappings) : -1

      for (let i = 0; i < neuronMeshes.length; i++) {
        const mesh = neuronMeshes[i]
        const modIdx = layout.moduleIndices[i] ?? 0
        const base = MODULE_COLORS[modIdx] ?? new THREE.Color('#9aa4b2')

        const dimmedByModule = modFilter >= 0 && modIdx !== modFilter
        const isSel = i === selNeuron
        const sharesPkg =
          selNeuron >= 0 &&
          selectedNode >= 0 &&
          layout.mappings[i]?.packageIndices.some(
            (p) =>
              (graph.nodes[selectedNode].c ?? []).includes(p) ||
              graph.nodes[selectedNode].p === p ||
              graph.nodes[p]?.pk === graph.nodes[selectedNode].pk,
          )

        const mat = mesh.material as THREE.MeshStandardMaterial
        if (!mat || !mat.isMeshStandardMaterial) continue

        if (isSel) {
          mat.emissiveIntensity = 2.0
          mat.emissive.setRGB(
            Math.min(1, base.r * 2.8 + 0.5),
            Math.min(1, base.g * 2.8 + 0.5),
            Math.min(1, base.b * 2.8 + 0.5),
          )
          mesh.scale.setScalar(1.5)
        } else if (dimmedByModule || (selNeuron >= 0 && !sharesPkg && !isSel)) {
          mat.emissiveIntensity = 0.15
          mat.emissive.copy(base).multiplyScalar(0.2)
          mesh.scale.setScalar(1)
        } else if (sharesPkg) {
          mat.emissiveIntensity = 1.2
          mat.emissive.copy(base).multiplyScalar(1.7)
          mesh.scale.setScalar(1.15)
        } else {
          mat.emissiveIntensity = 0.6
          mat.emissive.copy(base)
          mesh.scale.setScalar(1)
        }
      }
    }

    sceneRef.current = {
      neuronMeshes,
      glbScene,
      layout,
      mappings: layout.mappings,
      edgeGroup,
      controls,
      focus,
      setDimming,
    }

    // --- render loop ---
    const clock = new THREE.Clock()
    let raf = 0
    let disposed = false
    const loop = () => {
      if (disposed) return
      raf = requestAnimationFrame(loop)
      const dt = Math.min(clock.getDelta(), 0.05)
      if (!reducedMotion) synapses.update(dt)
      if (focusing) {
        controls.target.lerp(focusTarget, 0.08)
        if (controls.target.distanceTo(focusTarget) < 0.05) focusing = false
      }
      controls.update()
      composer.render()
    }
    loop()

    return () => {
      disposed = true
      cancelAnimationFrame(raf)
      ro.disconnect()
      controls.removeEventListener('start', stopAutoRotate)
      controls.dispose()
      renderer.domElement.removeEventListener('pointerdown', onPointerDown)
      renderer.domElement.removeEventListener('pointerup', onPointerUp)
      scene.traverse((obj) => {
        if (obj instanceof THREE.Mesh || obj instanceof THREE.Points) {
          obj.geometry.dispose()
          const m = obj.material as THREE.Material | THREE.Material[]
          if (Array.isArray(m)) m.forEach((x) => x.dispose())
          else m.dispose()
        }
      })
      composer.dispose()
      renderer.dispose()
      host.removeChild(renderer.domElement)
      sceneRef.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph, reducedMotion])

  /* -------- selection: recolor + axon bundle + fly-to -------- */
  useEffect(() => {
    const s = sceneRef.current
    if (!s) return

    const selNeuron = selected >= 0 ? findNeuronForNode(selected, graph, s.mappings) : -1
    s.setDimming(moduleFilter, selected)

    // Rebuild the selection edge bundle between neurons.
    s.edgeGroup.clear()
    if (selected >= 0) {
      const pairs = selectionEdges(graph, selected)
      if (pairs.length > 0) {
        const drawnEdges = new Set<string>()
        const verts: number[] = []
        for (const [a, b] of pairs) {
          const nA = findNeuronForNode(a, graph, s.mappings)
          const nB = findNeuronForNode(b, graph, s.mappings)
          if (nA < 0 || nB < 0 || nA === nB) continue
          const key = nA < nB ? `${nA}-${nB}` : `${nB}-${nA}`
          if (drawnEdges.has(key)) continue
          drawnEdges.add(key)

          const meshA = s.neuronMeshes[nA]
          const meshB = s.neuronMeshes[nB]
          if (!meshA || !meshB) continue

          const posA = new THREE.Vector3()
          const posB = new THREE.Vector3()
          meshA.getWorldPosition(posA)
          meshB.getWorldPosition(posB)
          verts.push(posA.x, posA.y, posA.z, posB.x, posB.y, posB.z)
        }
        if (verts.length > 0) {
          const geo = new THREE.BufferGeometry()
          geo.setAttribute('position', new THREE.BufferAttribute(new Float32Array(verts), 3))
          const mat = new THREE.LineBasicMaterial({
            color: new THREE.Color('#e8f0ff'),
            transparent: true,
            opacity: 0.5,
            blending: THREE.AdditiveBlending,
            depthWrite: false,
            toneMapped: false,
          })
          const lines = new THREE.LineSegments(geo, mat)
          lines.raycast = () => {}
          s.edgeGroup.add(lines)
        }
      }
      if (selNeuron >= 0) s.focus(selNeuron)
    }
  }, [graph, selected, moduleFilter])

  return <div ref={hostRef} className="absolute inset-0" aria-label="3D neural network" />
}
