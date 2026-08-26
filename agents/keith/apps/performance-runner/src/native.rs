use std::collections::BTreeSet;
use std::path::Path;
use std::time::Duration;

use keith_agent_types::{CURRENT_PROTOCOL_VERSION, ClientId, CommandId, SessionId, UtcTimestamp};
use keith_connection::{
    AgentTransport, FramedTransport, LocalStream, connect_local, set_local_read_timeout,
    set_local_write_timeout,
};
use keith_protocol::{
    ClientCommand, ClientHello, CommandEnvelope, CommandResult, CommandResultEnvelope, Feature,
    WireFormat, WireMessage,
};

pub struct NativeClient {
    transport: FramedTransport<LocalStream>,
    client_id: ClientId,
}

pub struct NativeResult {
    pub result: CommandResult,
    pub events: usize,
}

impl NativeClient {
    pub fn connect(socket: &Path, timeout: Duration) -> Result<Self, String> {
        let stream = connect_local(socket).map_err(|error| error.to_string())?;
        set_local_read_timeout(&stream, Some(timeout)).map_err(|error| error.to_string())?;
        set_local_write_timeout(&stream, Some(timeout)).map_err(|error| error.to_string())?;
        let mut transport = FramedTransport::new(stream, WireFormat::Json);
        let client_id = ClientId::new();
        transport
            .send(&WireMessage::ClientHello(ClientHello {
                protocol: CURRENT_PROTOCOL_VERSION,
                client_id: client_id.clone(),
                client_name: "keith-performance-runner".into(),
                client_version: env!("CARGO_PKG_VERSION").into(),
                supported_features: BTreeSet::from([
                    Feature::SessionLifecycle,
                    Feature::Goals,
                    Feature::Children,
                    Feature::Schedules,
                    Feature::MemoryQueries,
                    Feature::Replay,
                    Feature::Snapshots,
                    Feature::DeliveryDispatch,
                ]),
                resume: None,
            }))
            .map_err(|error| error.to_string())?;
        match transport.receive().map_err(|error| error.to_string())? {
            WireMessage::ServerHello(hello)
                if hello.protocol.major == CURRENT_PROTOCOL_VERSION.major => {}
            WireMessage::ServerHello(_) => {
                return Err("daemon negotiated an incompatible protocol".into());
            }
            _ => return Err("daemon omitted ServerHello".into()),
        }
        Ok(Self {
            transport,
            client_id,
        })
    }

    pub fn command(
        &mut self,
        session_id: Option<SessionId>,
        command: ClientCommand,
    ) -> Result<NativeResult, String> {
        let command_id = CommandId::new();
        self.transport
            .send(&WireMessage::Command(CommandEnvelope {
                protocol: CURRENT_PROTOCOL_VERSION,
                command_id: command_id.clone(),
                client_id: self.client_id.clone(),
                sent_at: UtcTimestamp::now().map_err(|error| error.to_string())?,
                session_id,
                command,
            }))
            .map_err(|error| error.to_string())?;
        let mut events = 0_usize;
        loop {
            match self
                .transport
                .receive()
                .map_err(|error| error.to_string())?
            {
                WireMessage::Event(_) | WireMessage::Snapshot(_) | WireMessage::Terminal(_) => {
                    events = events.saturating_add(1);
                }
                WireMessage::CommandResult(CommandResultEnvelope {
                    command_id: returned,
                    result,
                    ..
                }) if returned == command_id => return Ok(NativeResult { result, events }),
                WireMessage::CommandResult(_) | WireMessage::ServerHello(_) => {}
                WireMessage::ClientHello(_) | WireMessage::Command(_) => {
                    return Err("daemon returned a client-side wire message".into());
                }
            }
        }
    }
}
