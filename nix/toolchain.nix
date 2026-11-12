{
  perSystem = { inputs', ... }: {
    _module.args.go = inputs'.devtools.packages.go;
  };
}
