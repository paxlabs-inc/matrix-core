<p align="center">
  <img src="https://pub-cc719bc237f94810bec78e93e056bec4.r2.dev/centra.ai_wordmark_dark.png" alt="Centra AI" width="520" />
</p>

<p align="center">
  <strong>Un sistema operativo de IA con estado para trabajo serio y de larga duración.</strong>
</p>

<p align="center">
  Centra AI proporciona a los agentes memoria, herramientas, ordenadores aislados,
  trabajo coordinado y un registro duradero de lo que realmente hicieron.
</p>

<p align="center">
  <a href="https://github.com/Sidiora-Labs/centra-llm-agents">Repositorio</a> ·
  <a href="README.md">English</a> ·
  <a href="LICENSE.md">Licencia Centra AI Protocol</a>
</p>

## ¿Qué es Centra AI?

Centra AI es una plataforma privada y persistente de agentes creada por Sidiora Labs. Está diseñada para trabajos que no caben de forma fiable en un solo prompt: construir software, investigar preguntas complejas, operar navegadores y archivos, coordinar especialistas en paralelo, producir artefactos y continuar entre sesiones sin perder el estado del trabajo.

El sistema se centra en dos agentes:

- **Neo** es el agente principal. Investiga, razona, opera herramientas, crea artefactos, gestiona trabajo activo y permanece con una tarea más allá de una sola respuesta del modelo.
- **Ion** es el agente técnico y el entorno de programación. Trabaja directamente con proyectos reales, terminales, archivos, pruebas, vistas previas y toolchains dentro de un workspace acotado.

Los respalda **Workforce**, la capa de ejecución coordinada para dividir objetivos grandes en trabajo paralelo gobernado, y **Neo Computer**, el lugar unificado para fuentes, vistas previas, artefactos, cambios y evidencia del workspace.

La conversación es la superficie de control; el producto es el sistema de trabajo que existe detrás de ella.

## Qué lo hace diferente

- **Cognición persistente.** Identidad, memoria, objetivos, evidencia y trabajo pendiente sobreviven a reinicios y cambios de ventana de contexto.
- **Entornos de ejecución reales.** Los agentes trabajan con repositorios, terminales, navegadores, servicios, bases de datos, herramientas multimedia y sistemas externos.
- **Evidencia antes que apariencia.** Fuentes, citas, resultados de herramientas, artefactos, checkpoints y verificación permanecen unidos al trabajo.
- **Trabajo de larga duración.** Las tareas se pueden dividir, poner en cola, corregir, reanudar y continuar mediante especialistas acotados.
- **Control humano.** Las operaciones sensibles o con efectos externos pasan por límites explícitos de autoridad y aprobación.
- **Aislamiento por usuario.** Cada usuario obtiene un entorno de agente y un límite de estado duradero dedicados.

## Capacidades principales

| Área | Capacidades |
| --- | --- |
| Ingeniería de software | Programación consciente del repositorio, shell, paquetes, pruebas, servicios, diffs, previews y entrega de proyectos. |
| Investigación | Investigación multifuente, citas exactas, extractos, síntesis y artefactos duraderos. |
| Uso del ordenador | Operación de navegador y escritorio con evidencia visible y autoridad acotada. |
| Memoria | Identidad, preferencias, hechos, objetivos, continuidad y creencias vinculadas a evidencia. |
| Trabajo coordinado | Descomposición, especialistas, paralelismo acotado, supervisión y reanudación. |
| Automatización | Trabajo programado, ejecución bajo demanda, colas duraderas y entregas proactivas. |

## Inicio rápido

```bash
git clone https://github.com/Sidiora-Labs/centra-llm-agents.git
cd centra-llm-agents
make build
make install
```

Cliente:

```bash
cd client
corepack enable
pnpm install
pnpm dev
```

## Documentación

- [Arquitectura](ARCHITECTURE.md)
- [Contrato de marca y compatibilidad](BRANDING.md)
- [Contribución](CONTRIBUTING.md)
- [Seguridad](SECURITY.md)
- [Cómo se construye Centra AI](HOW_CENTRA_AI_WAS_BUILT.md)
- [Documentación completa](docs/)

## Licencia

Centra AI está disponible bajo la [Licencia Centra AI Protocol](LICENSE.md).

```text
Copyright © 2026 Sidiora Labs. All rights reserved.
SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
```
