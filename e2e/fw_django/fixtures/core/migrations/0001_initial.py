from django.db import migrations, models


class Migration(migrations.Migration):
    initial = True
    dependencies = []
    operations = [
        migrations.CreateModel(
            name="Widget",
            fields=[
                ("id", models.AutoField(primary_key=True, serialize=False)),
                ("name", models.CharField(max_length=64)),
                ("price", models.DecimalField(max_digits=10, decimal_places=2)),
            ],
        ),
    ]
