class CreateWidgets < ActiveRecord::Migration[7.1]
  def change
    create_table :widgets do |t|
      t.string :name, null: false
      t.decimal :price, precision: 10, scale: 2, null: false
    end
  end
end
