import os

SECRET_KEY = "treeman-e2e"
DEBUG = True
INSTALLED_APPS = [
    "django.contrib.contenttypes",
    "django.contrib.auth",
    "core",
]
DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "NAME": os.environ["DJANGO_DB_NAME"],
        "USER": os.environ.get("DJANGO_DB_USER", "postgres"),
        "PASSWORD": os.environ.get("DJANGO_DB_PASSWORD", "pgpw"),
        "HOST": os.environ.get("DJANGO_DB_HOST", "postgres"),
        "PORT": int(os.environ.get("DJANGO_DB_PORT", 5432)),
    }
}
USE_TZ = True
DEFAULT_AUTO_FIELD = "django.db.models.AutoField"
