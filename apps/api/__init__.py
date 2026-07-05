"""FastAPI surface for the local Rushes API."""

from .main import create_app, create_app_from_env

__all__ = ["create_app", "create_app_from_env"]
