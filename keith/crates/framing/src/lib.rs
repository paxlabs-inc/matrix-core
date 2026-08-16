#![forbid(unsafe_code)]

use std::io::{self, Read, Write};

use thiserror::Error;

pub const DEFAULT_MAX_FRAME_BYTES: usize = 16 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LengthDelimitedCodec {
    max_frame_bytes: usize,
}

impl LengthDelimitedCodec {
    /// # Errors
    ///
    /// Returns [`FrameError::InvalidLimit`] when the limit is zero or exceeds `u32` framing.
    pub fn new(max_frame_bytes: usize) -> Result<Self, FrameError> {
        if max_frame_bytes == 0 || u32::try_from(max_frame_bytes).is_err() {
            Err(FrameError::InvalidLimit(max_frame_bytes))
        } else {
            Ok(Self { max_frame_bytes })
        }
    }

    pub const fn max_frame_bytes(self) -> usize {
        self.max_frame_bytes
    }

    /// # Errors
    ///
    /// Returns an I/O or size error if the complete frame cannot be written.
    pub fn write_frame<W: Write>(self, writer: &mut W, payload: &[u8]) -> Result<(), FrameError> {
        if payload.len() > self.max_frame_bytes {
            return Err(FrameError::TooLarge {
                length: payload.len(),
                limit: self.max_frame_bytes,
            });
        }
        let length = u32::try_from(payload.len()).map_err(|_| FrameError::TooLarge {
            length: payload.len(),
            limit: self.max_frame_bytes,
        })?;
        writer.write_all(&length.to_be_bytes())?;
        writer.write_all(payload)?;
        writer.flush()?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an I/O, truncation, or size error if a complete valid frame cannot be read.
    pub fn read_frame<R: Read>(self, reader: &mut R) -> Result<Option<Vec<u8>>, FrameError> {
        let mut header = [0_u8; 4];
        match reader.read(&mut header[..1]) {
            Ok(0) => return Ok(None),
            Ok(1) => {}
            Ok(_) => unreachable!("one-byte read returned more than one byte"),
            Err(error) => return Err(FrameError::Io(error)),
        }
        reader
            .read_exact(&mut header[1..])
            .map_err(|error| truncated(error, "frame header"))?;
        let wire_length = u32::from_be_bytes(header);
        let length = usize::try_from(wire_length)
            .map_err(|_| FrameError::UnsupportedPlatformLength(wire_length))?;
        if length > self.max_frame_bytes {
            return Err(FrameError::TooLarge {
                length,
                limit: self.max_frame_bytes,
            });
        }
        let mut payload = vec![0_u8; length];
        reader
            .read_exact(&mut payload)
            .map_err(|error| truncated(error, "frame payload"))?;
        Ok(Some(payload))
    }
}

impl Default for LengthDelimitedCodec {
    fn default() -> Self {
        Self {
            max_frame_bytes: DEFAULT_MAX_FRAME_BYTES,
        }
    }
}

#[derive(Debug, Error)]
pub enum FrameError {
    #[error("frame I/O failed: {0}")]
    Io(#[from] io::Error),
    #[error("frame length {length} exceeds limit {limit}")]
    TooLarge { length: usize, limit: usize },
    #[error("invalid frame limit {0}")]
    InvalidLimit(usize),
    #[error("frame length {0} is unsupported on this platform")]
    UnsupportedPlatformLength(u32),
    #[error("truncated {context}: {source}")]
    Truncated {
        context: &'static str,
        source: io::Error,
    },
}

fn truncated(error: io::Error, context: &'static str) -> FrameError {
    if error.kind() == io::ErrorKind::UnexpectedEof {
        FrameError::Truncated {
            context,
            source: error,
        }
    } else {
        FrameError::Io(error)
    }
}

#[cfg(test)]
mod tests {
    use std::io::Cursor;

    use super::*;

    #[test]
    fn frames_round_trip_and_eof_is_explicit() {
        let codec = LengthDelimitedCodec::new(32).unwrap();
        let mut bytes = Vec::new();
        codec.write_frame(&mut bytes, b"keith").unwrap();
        let mut reader = Cursor::new(bytes);
        assert_eq!(
            codec.read_frame(&mut reader).unwrap(),
            Some(b"keith".to_vec())
        );
        assert_eq!(codec.read_frame(&mut reader).unwrap(), None);
    }

    #[test]
    fn oversized_and_truncated_frames_fail_before_success() {
        let codec = LengthDelimitedCodec::new(4).unwrap();
        assert!(matches!(
            codec.write_frame(&mut Vec::new(), b"large"),
            Err(FrameError::TooLarge { .. })
        ));
        let mut oversized = Cursor::new(5_u32.to_be_bytes());
        assert!(matches!(
            codec.read_frame(&mut oversized),
            Err(FrameError::TooLarge { .. })
        ));
        let mut truncated = Cursor::new([0, 0, 0, 2, 1]);
        assert!(matches!(
            codec.read_frame(&mut truncated),
            Err(FrameError::Truncated { .. })
        ));
    }
}
